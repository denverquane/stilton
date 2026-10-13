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
	"slices"
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
	// Moved files were moved or renamed outside STLton: same contents at a
	// new path. They keep their tags, source links, and download history.
	Moved int `json:"moved"`
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
//
// Files that vanished from one path while a file with the same contents
// appeared at another were moved outside STLton; their index rows are
// relocated rather than deleted and recreated. When a whole folder moved or
// was renamed, its tags, source link, and download history follow it.
func (s *Scanner) scan(ctx context.Context) (ScanResult, error) {
	var res ScanResult
	known, err := s.DB.KnownFiles(ctx)
	if err != nil {
		return res, err
	}
	before, err := s.DB.CollectionIDs(ctx)
	if err != nil {
		return res, err
	}
	seen := make(map[string]bool, len(known))
	collections := collectionCache{db: s.DB, ids: map[string]int64{"": db.RootCollectionID}}
	var added []db.Model // new paths; inserted once moves are matched

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
		m := db.Model{
			CollectionID: collectionID,
			Path:         rel,
			Kind:         kind,
			Name:         name,
			Format:       format,
			ThumbState:   thumb,
			Size:         info.Size(),
			Mtime:        info.ModTime().Unix(),
			Hash:         hash,
		}
		if !exists {
			added = append(added, m)
			return nil
		}
		if err := s.DB.UpsertModel(ctx, m); err != nil {
			return err
		}
		res.Updated++
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

	// Pair each new path with a vanished one holding the same contents.
	missingByHash := map[string][]string{}
	for p, fi := range known {
		if !seen[p] {
			missingByHash[fi.Hash] = append(missingByHash[fi.Hash], p)
		}
	}
	for _, paths := range missingByHash {
		slices.Sort(paths) // deterministic pairing among identical files
	}
	slices.SortFunc(added, func(a, b db.Model) int { return strings.Compare(a.Path, b.Path) })
	moves := map[string]string{} // old path -> new path
	for _, m := range added {
		if from, ok := takeMatch(missingByHash, m); ok {
			if err := s.DB.RelocateModel(ctx, known[from].ID, m); err != nil {
				return res, err
			}
			moves[from] = m.Path
			res.Moved++
			continue
		}
		if err := s.DB.UpsertModel(ctx, m); err != nil {
			return res, err
		}
		res.Added++
	}

	var missing []string
	for p := range known {
		if !seen[p] && moves[p] == "" {
			missing = append(missing, p)
		}
	}
	if err := s.DB.DeleteModelsByPath(ctx, missing); err != nil {
		return res, err
	}
	res.Removed = len(missing)

	if len(moves) > 0 {
		for from, to := range folderMoves(known, seen, moves, before, collections.ids) {
			if err := s.DB.TransferCollection(ctx, before[from], collections.ids[to]); err != nil {
				return res, err
			}
			slog.Info("folder moved", "from", from, "to", to)
		}
		slog.Info("files moved", "count", res.Moved)
	}
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

// takeMatch removes and returns a vanished path with m's contents,
// preferring one with the same file name (a folder move) over a rename.
func takeMatch(missingByHash map[string][]string, m db.Model) (string, bool) {
	paths := missingByHash[m.Hash]
	if len(paths) == 0 {
		return "", false
	}
	i := slices.IndexFunc(paths, func(p string) bool { return path.Base(p) == path.Base(m.Path) })
	if i < 0 {
		i = 0
	}
	from := paths[i]
	missingByHash[m.Hash] = slices.Delete(paths, i, i+1)
	return from, true
}

// folderMoves works out which folders were moved or renamed as a whole, from
// the files that moved. A folder counts as moved to a new folder when it's
// now empty, everything that was directly in it moved to that one new
// folder, and the new folder didn't exist before this scan. A folder with
// only subfolders counts as moved when all of them moved into one new
// parent. Returns old path -> new path.
func folderMoves(known map[string]db.FileInfo, seen map[string]bool, moves map[string]string, before, after map[string]int64) map[string]string {
	// Folders that still contain something on disk aren't moved.
	occupied := map[string]bool{}
	for p := range seen {
		for d := dirOf(p); ; d = dirOf(d) {
			occupied[d] = true
			if d == "" {
				break
			}
		}
	}
	isNew := func(dir string) bool {
		_, existed := before[dir]
		_, exists := after[dir]
		return dir != "" && exists && !existed
	}

	// Where each old folder's direct files went, and whether any didn't move.
	dest := map[string]map[string]bool{}
	direct := map[string]bool{}
	for p := range known {
		dir := dirOf(p)
		direct[dir] = true
		if dest[dir] == nil {
			dest[dir] = map[string]bool{}
		}
		if to, ok := moves[p]; ok {
			dest[dir][dirOf(to)] = true
		} else {
			dest[dir]["\x00"] = true // deleted, not moved
		}
	}

	// Deepest first, so parents can be decided from their children.
	var folders []string
	for dir := range before {
		if dir != "" && !occupied[dir] {
			folders = append(folders, dir)
		}
	}
	slices.SortFunc(folders, func(a, b string) int {
		if d := strings.Count(b, "/") - strings.Count(a, "/"); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})

	out := map[string]string{}
	for _, dir := range folders {
		var target string
		if direct[dir] {
			if len(dest[dir]) != 1 {
				continue
			}
			for t := range dest[dir] {
				target = t
			}
		} else {
			// Only subfolders: they must all have moved into one parent.
			parents := map[string]bool{}
			children := 0
			for child := range before {
				if child != "" && dirOf(child) == dir {
					children++
					if to, ok := out[child]; ok {
						parents[dirOf(to)] = true
					} else {
						parents["\x00"] = true
					}
				}
			}
			if children == 0 || len(parents) != 1 {
				continue
			}
			for p := range parents {
				target = p
			}
		}
		if isNew(target) {
			out[dir] = target
		}
	}
	return out
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
