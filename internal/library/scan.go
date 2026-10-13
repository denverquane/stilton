// Package library indexes model files under the library root into the database.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/denverquane/stilton/internal/db"
)

var formats = map[string]string{
	".stl": "stl",
	".obj": "obj",
	".3mf": "3mf",
}

// imageFormats can be decoded into thumbnails.
var imageFormats = map[string]bool{"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "bmp": true}

// IsImage reports whether a file format (lowercase extension) is an image we
// can preview.
func IsImage(format string) bool { return imageFormats[format] }

// classify describes a file for the index: its kind, format, display name,
// and initial thumbnail state. Models are named without their extension;
// other files keep it, since it's what tells them apart.
func classify(filename string) (kind, format, name, thumb string) {
	if format = FormatOf(filename); format != "" {
		return db.KindModel, format, strings.TrimSuffix(filename, filepath.Ext(filename)), db.ThumbPending
	}
	format = strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	thumb = db.ThumbNone
	if IsImage(format) {
		thumb = db.ThumbPending
	}
	return db.KindFile, format, filename, thumb
}

// FormatOf returns the model format for a filename, or "" if unsupported.
func FormatOf(name string) string {
	return formats[strings.ToLower(filepath.Ext(name))]
}

type Scanner struct {
	Root string
	DB   *db.DB
	// OnChange is called after a scan that added or updated at least one model.
	OnChange func()
	// OnRemoved is called (in its own goroutine) after models leave the index
	// or trash is purged, so unused thumbnails can be swept.
	OnRemoved func()

	mu sync.Mutex // serializes scans
}

type ScanResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
}

// Scan walks the library, hashing new or changed files (models and every
// other non-hidden file) and removing rows for
// files that no longer exist. Unchanged files (same size and mtime) are skipped.
// Each model belongs to the collection for its folder; collections left with
// no models beneath them are removed.
func (s *Scanner) Scan(ctx context.Context) (ScanResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scan(ctx)
}

// scan is Scan for callers already holding s.mu.
func (s *Scanner) scan(ctx context.Context) (ScanResult, error) {

	var res ScanResult
	known, err := s.DB.KnownFiles(ctx)
	if err != nil {
		return res, err
	}
	seen := make(map[string]bool, len(known))
	collections := collectionCache{db: s.DB, ids: map[string]int64{"": db.RootCollectionID}}

	err = filepath.WalkDir(s.Root, func(fullPath string, d fs.DirEntry, err error) error {
		if err != nil {
			if fullPath == s.Root {
				return err // abort rather than treat an unreadable library as empty
			}
			slog.Warn("scan: skipping", "path", fullPath, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.HasPrefix(d.Name(), ".") && fullPath != s.Root {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		kind, format, name, thumb := classify(d.Name())

		rel, err := filepath.Rel(s.Root, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true

		info, err := d.Info()
		if err != nil {
			slog.Warn("scan: stat failed", "path", fullPath, "err", err)
			return nil
		}
		prev, exists := known[rel]
		if exists && prev.Size == info.Size() && prev.Mtime == info.ModTime().Unix() {
			return nil
		}

		hash, err := hashFile(fullPath)
		if err != nil {
			slog.Warn("scan: hash failed", "path", fullPath, "err", err)
			return nil
		}
		collectionID, err := collections.idFor(ctx, dirOf(rel))
		if err != nil {
			return err
		}
		err = s.DB.UpsertModel(ctx, db.Model{
			CollectionID: collectionID,
			Path:         rel,
			Kind:         kind,
			Name:         name,
			Format:       format,
			ThumbState:   thumb,
			Size:         info.Size(),
			Mtime:        info.ModTime().Unix(),
			Hash:         hash,
		})
		if err != nil {
			return err
		}
		if exists {
			res.Updated++
		} else {
			res.Added++
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	// An unmounted network/USB mount point looks like an empty directory.
	// Deleting everything would also drop all tags, so refuse.
	if len(seen) == 0 && len(known) > 0 {
		return res, fmt.Errorf("library %s is empty but %d models are indexed; is it mounted? not removing anything", s.Root, len(known))
	}

	var missing []string
	for p := range known {
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	if err := s.DB.DeleteModelsByPath(ctx, missing); err != nil {
		return res, err
	}
	res.Removed = len(missing)
	if _, err := s.DB.PruneCollections(ctx); err != nil {
		return res, err
	}
	if err := s.DB.PruneTags(ctx); err != nil {
		return res, err
	}

	if (res.Added > 0 || res.Updated > 0) && s.OnChange != nil {
		s.OnChange()
	}
	if res.Removed > 0 || res.Updated > 0 {
		s.removed()
	}
	return res, nil
}

// removed notifies OnRemoved, if set.
func (s *Scanner) removed() {
	if s.OnRemoved != nil {
		go s.OnRemoved()
	}
}

// collectionCache resolves folder paths to collection ids, creating
// collections (and their ancestors) on first use.
type collectionCache struct {
	db  *db.DB
	ids map[string]int64
}

func (c *collectionCache) idFor(ctx context.Context, dir string) (int64, error) {
	if id, ok := c.ids[dir]; ok {
		return id, nil
	}
	parentID, err := c.idFor(ctx, dirOf(dir))
	if err != nil {
		return 0, err
	}
	id, err := c.db.UpsertCollection(ctx, parentID, dir, path.Base(dir))
	if err != nil {
		return 0, err
	}
	c.ids[dir] = id
	return id, nil
}

// dirOf returns the slash-separated parent folder of rel, or "" at the root.
func dirOf(rel string) string {
	if dir := path.Dir(rel); dir != "." {
		return dir
	}
	return ""
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
