// Package thumbnail renders model previews and stores them as PNGs keyed by
// content hash, so identical files share one thumbnail and URLs never go stale.
package thumbnail

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fogleman/fauxgl"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/denverquane/stilton/internal/db"
	"github.com/denverquane/stilton/internal/mesh"
)

const (
	Size = 384
	// Render at a multiple of Size and downsample for cheap antialiasing.
	supersample = 2
)

var (
	fovy    = 30.0
	viewDir = fauxgl.V(2, -3, 2.2).Normalize() // front-right and above; models are Z-up
	center  = fauxgl.V(0, 0, 0)
	up      = fauxgl.V(0, 0, 1)
	light   = fauxgl.V(1, -1.5, 2).Normalize()

	background = fauxgl.HexColor("#1e2127")
	modelColor = fauxgl.HexColor("#e8a33d")
)

// Render draws a mesh centered in a square frame.
func Render(m *fauxgl.Mesh) image.Image {
	m.BiUnitCube()
	m.SmoothNormalsThreshold(fauxgl.Radians(30))

	// Back the camera off just far enough for the mesh's bounding sphere to fit.
	var radius float64
	for _, t := range m.Triangles {
		for _, v := range [3]fauxgl.Vector{t.V1.Position, t.V2.Position, t.V3.Position} {
			radius = max(radius, v.Length())
		}
	}
	eye := viewDir.MulScalar(1.05 * radius / math.Sin(fauxgl.Radians(fovy/2)))

	const px = Size * supersample
	ctx := fauxgl.NewContext(px, px)
	ctx.ClearColorBufferWith(background)

	matrix := fauxgl.LookAt(eye, center, up).Perspective(fovy, 1, 0.1, 20)
	shader := fauxgl.NewPhongShader(matrix, light, eye)
	shader.ObjectColor = modelColor
	shader.AmbientColor = fauxgl.Gray(0.35)
	shader.SpecularColor = fauxgl.Gray(0.25)
	shader.SpecularPower = 24
	ctx.Shader = shader
	ctx.DrawMesh(m)

	return downsample(ctx.Image(), Size)
}

func downsample(src image.Image, size int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// fit scales an embedded slicer thumbnail into a square frame, preserving aspect.
func fit(src image.Image, size int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{background.NRGBA()}, image.Point{}, draw.Src)
	b := src.Bounds()
	w, h := size, size
	if b.Dx() > b.Dy() {
		h = size * b.Dy() / b.Dx()
	} else {
		w = size * b.Dx() / b.Dy()
	}
	r := image.Rect((size-w)/2, (size-h)/2, (size-w)/2+w, (size-h)/2+h)
	draw.CatmullRom.Scale(dst, r, src, b, draw.Over, nil)
	return dst
}

// Generator renders thumbnails for pending models in the background.
type Generator struct {
	db          *db.DB
	libraryRoot string
	dir         string // output directory for <hash>.png
	workers     int
	wake        chan struct{}
	sweepMu     sync.Mutex
}

func NewGenerator(database *db.DB, libraryRoot, dir string, workers int) *Generator {
	return &Generator{
		db:          database,
		libraryRoot: libraryRoot,
		dir:         dir,
		workers:     max(workers, 1),
		wake:        make(chan struct{}, 1),
	}
}

func (g *Generator) Path(hash string) string {
	return filepath.Join(g.dir, hash+".png")
}

// Wake asks the generator to look for pending work. It never blocks.
func (g *Generator) Wake() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Run processes pending thumbnails until ctx is cancelled.
func (g *Generator) Run(ctx context.Context) error {
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	for {
		g.drain(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.wake:
		}
	}
}

// drain renders batches of pending models until none are left.
func (g *Generator) drain(ctx context.Context) {
	for ctx.Err() == nil {
		pending, err := g.db.PendingThumbnails(ctx, 64)
		if err != nil {
			slog.Error("thumbnail: query pending", "err", err)
			return
		}
		if len(pending) == 0 {
			return
		}

		// Identical files share a hash; render each hash once per batch.
		jobs := make(chan db.Model)
		var wg sync.WaitGroup
		var saved atomic.Int32
		for range g.workers {
			wg.Go(func() {
				for m := range jobs {
					if g.process(ctx, m) {
						saved.Add(1)
					}
				}
			})
		}
		seen := map[string]bool{}
		for _, m := range pending {
			if !seen[m.Hash] {
				seen[m.Hash] = true
				jobs <- m
			}
		}
		close(jobs)
		wg.Wait()
		if saved.Load() == 0 {
			return // no state could be recorded; wait for the next wake instead of spinning
		}
	}
}

// process renders one thumbnail and reports whether its state was saved.
func (g *Generator) process(ctx context.Context, m db.Model) bool {
	state, msg := db.ThumbOK, ""
	if err := g.generate(m); err != nil {
		state, msg = db.ThumbFailed, err.Error()
		slog.Warn("thumbnail failed", "path", m.Path, "err", err)
	}
	if err := g.db.SetThumbState(ctx, m.Hash, state, msg); err != nil {
		slog.Error("thumbnail: save state", "path", m.Path, "err", err)
		return false
	}
	return true
}

func (g *Generator) generate(m db.Model) error {
	out := g.Path(m.Hash)
	if _, err := os.Stat(out); err == nil {
		return nil // already rendered for an identical file
	}

	src := filepath.Join(g.libraryRoot, filepath.FromSlash(m.Path))
	if m.Kind == db.KindFile {
		img, err := loadImage(src)
		if err != nil {
			return err
		}
		return writePNG(out, fit(img, Size))
	}
	var img image.Image
	if m.Format == "3mf" {
		if embedded, ok := mesh.EmbeddedThumbnail(src); ok {
			img = fit(embedded, Size)
		}
	}
	if img == nil {
		mm, err := mesh.Load(src, m.Format)
		if err != nil {
			return err
		}
		img = Render(mm)
	}
	return writePNG(out, img)
}

// maxImagePixels bounds decoded image size; a 30k×30k photo would need gigabytes.
const maxImagePixels = 100_000_000

// loadImage decodes an image file for a thumbnail.
func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return nil, fmt.Errorf("image is too large to preview (%d×%d)", cfg.Width, cfg.Height)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(f)
	return img, err
}

// Sweep deletes thumbnails whose hash isn't in the set returned by keep,
// and returns how many it removed. The directory is listed before keep is
// called, so a thumbnail rendered for a model indexed meanwhile is never
// mistaken for an orphan.
func (g *Generator) Sweep(ctx context.Context, keep func(context.Context) (map[string]bool, error)) (int, error) {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()

	entries, err := os.ReadDir(g.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	keepSet, err := keep(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		hash, ok := strings.CutSuffix(e.Name(), ".png")
		if !ok || len(hash) != 64 || keepSet[hash] {
			continue // not a thumbnail (e.g. an in-progress temp file), or still used
		}
		if err := os.Remove(filepath.Join(g.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// writePNG writes atomically so a crash never leaves a truncated thumbnail
// that the browser would then cache forever.
func writePNG(path string, img image.Image) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".thumb-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := png.Encode(tmp, img); err != nil {
		tmp.Close()
		return fmt.Errorf("encode png: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
