package library

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/denverquane/stilton/internal/db"
)

// TrashDir is the hidden folder at the library root that deleted items are
// moved into. It's on the same filesystem as the library, so trashing and
// restoring are renames however large the files are. The scanner skips it
// because it's hidden.
const TrashDir = ".stilton-trash"

// A trash batch is one deletion: <TrashDir>/<id>/manifest.json, with the
// deleted items under <TrashDir>/<id>/files/<original path>.
var batchID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

var ErrBatchNotFound = errors.New("no such trash batch")

// TrashBatch describes one deletion that can be restored.
type TrashBatch struct {
	ID        string       `json:"id"`
	DeletedAt int64        `json:"deletedAt"`
	Reason    string       `json:"reason"` // e.g. "Deleted", "Duplicate of …"
	Items     []DeleteItem `json:"items"`
	Files     int          `json:"files"`
	Bytes     int64        `json:"bytes"`
	// What the index knew about the deleted items, so a restore brings back
	// tags and source links, and thumbnails survive the sweep.
	Collections []TrashedMeta `json:"collections"`
	Models      []TrashedMeta `json:"models"`
}

// TrashedMeta is a deleted model's or collection's index metadata.
type TrashedMeta struct {
	Path      string   `json:"path"`
	Hash      string   `json:"hash,omitempty"`
	Up        string   `json:"up,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	SourceURL string   `json:"sourceUrl,omitempty"`
}

func (s *Scanner) trashRoot() string { return filepath.Join(s.Root, TrashDir) }

func (s *Scanner) batchDir(id string) string { return filepath.Join(s.trashRoot(), id) }

// trashSelection moves the selection into a new trash batch and removes it
// from the index. Items are moved one at a time; if one fails, the batch and
// index reflect what was moved and the error says how far it got.
func (s *Scanner) trashSelection(ctx context.Context, sel selection, items []DeleteItem, reason string) (TrashBatch, error) {
	suffix := make([]byte, 3)
	rand.Read(suffix)
	now := time.Now()
	b := TrashBatch{
		ID:        now.Format("20060102-150405") + "-" + hex.EncodeToString(suffix),
		DeletedAt: now.Unix(),
		Reason:    reason,
	}
	for _, c := range sel.collections {
		cs, ms, err := s.DB.Subtree(ctx, c.Path)
		if err != nil {
			return b, err
		}
		for _, c := range cs {
			b.Collections = append(b.Collections, TrashedMeta{Path: c.Path, Tags: c.Tags, SourceURL: c.SourceURL})
		}
		for _, m := range ms {
			b.Models = append(b.Models, TrashedMeta{Path: m.Path, Hash: m.Hash, Up: m.Up, Tags: m.Tags, SourceURL: m.SourceURL})
		}
	}
	for _, m := range sel.models {
		b.Models = append(b.Models, TrashedMeta{Path: m.Path, Hash: m.Hash, Up: m.Up, Tags: m.Tags, SourceURL: m.SourceURL})
	}

	dir := s.batchDir(b.ID)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return b, err
	}
	var doneModels []int64
	var doneCollections []db.Collection
	var sources []string
	move := func(rel string, it DeleteItem) error {
		from := s.abs(rel)
		to := filepath.Join(dir, "files", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		b.Items = append(b.Items, it)
		b.Files += it.Files
		b.Bytes += it.Bytes
		sources = append(sources, filepath.Dir(from))
		return nil
	}

	var failure error
	itemFor := func(rel string) DeleteItem {
		i := slices.IndexFunc(items, func(it DeleteItem) bool { return it.Path == rel })
		if i < 0 {
			return DeleteItem{Path: rel}
		}
		return items[i]
	}
	for _, c := range sel.collections {
		if err := move(c.Path, itemFor(c.Path)); err != nil {
			failure = fmt.Errorf("moved %d of %d items to the trash, then failed on %q: %w; rescan to check", len(b.Items), len(items), c.Path, err)
			break
		}
		doneCollections = append(doneCollections, c)
	}
	if failure == nil {
		for _, m := range sel.models {
			if err := move(m.Path, itemFor(m.Path)); err != nil {
				failure = fmt.Errorf("moved %d of %d items to the trash, then failed on %q: %w; rescan to check", len(b.Items), len(items), m.Path, err)
				break
			}
			doneModels = append(doneModels, m.ID)
		}
	}

	errs := []error{failure, writeManifest(dir, b), s.DB.DeleteItems(ctx, doneModels, doneCollections)}
	for _, d := range sources {
		s.removeEmptyDirs(d)
	}
	_, err := s.DB.PruneCollections(ctx)
	errs = append(errs, err, s.DB.PruneTags(ctx))
	s.removed()
	if len(b.Items) == 0 {
		os.RemoveAll(dir) // nothing made it in
	}
	slog.Info("moved to trash", "batch", b.ID, "items", len(b.Items), "files", b.Files)
	return b, errors.Join(errs...)
}

func writeManifest(dir string, b TrashBatch) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

func (s *Scanner) readBatch(id string) (TrashBatch, error) {
	var b TrashBatch
	if !batchID.MatchString(id) {
		return b, ErrBatchNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.batchDir(id), "manifest.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return b, ErrBatchNotFound
	}
	if err != nil {
		return b, err
	}
	return b, json.Unmarshal(data, &b)
}

// Trash lists batches in the trash, newest first.
func (s *Scanner) Trash() ([]TrashBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listTrash()
}

func (s *Scanner) listTrash() ([]TrashBatch, error) {
	entries, err := os.ReadDir(s.trashRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return []TrashBatch{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []TrashBatch{}
	for _, e := range entries {
		if !e.IsDir() || !batchID.MatchString(e.Name()) {
			continue
		}
		b, err := s.readBatch(e.Name())
		if err != nil {
			slog.Warn("unreadable trash batch", "id", e.Name(), "err", err)
			continue
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b TrashBatch) int { return int(b.DeletedAt - a.DeletedAt) })
	return out, nil
}

// TrashThumbKeys returns the thumbnail keys of models in the trash, whose
// thumbnails should be kept for a possible restore.
func (s *Scanner) TrashThumbKeys() (map[string]bool, error) {
	batches, err := s.Trash()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, b := range batches {
		for _, m := range b.Models {
			out[db.ThumbKey(m.Hash, m.Up)] = true
		}
	}
	return out, nil
}

// RestoreTrash moves a batch's items back to where they were, indexes them,
// and reapplies their tags and source links. Nothing moves if any original
// location is now occupied.
func (s *Scanner) RestoreTrash(ctx context.Context, id string) (TrashBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.readBatch(id)
	if err != nil {
		return b, err
	}
	dir := s.batchDir(id)
	for _, it := range b.Items {
		if _, err := os.Lstat(s.abs(it.Path)); err == nil {
			return b, conflict("%q exists again in the library; move or rename it first", it.Path)
		}
	}

	type move struct{ from, to string }
	var done []move
	for _, it := range b.Items {
		from := filepath.Join(dir, "files", filepath.FromSlash(it.Path))
		to := s.abs(it.Path)
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue // never made it into the trash
		}
		err := os.MkdirAll(filepath.Dir(to), 0o755)
		if err == nil {
			err = os.Rename(from, to)
		}
		if err != nil {
			for i := len(done) - 1; i >= 0; i-- {
				os.Rename(done[i].to, done[i].from)
			}
			return b, err
		}
		done = append(done, move{from, to})
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("removing restored trash batch", "id", id, "err", err)
	}

	if _, err := s.scan(ctx); err != nil {
		return b, fmt.Errorf("restored the files, but indexing them failed: %w", err)
	}
	// Reapply metadata. Tag updates also recreate tags pruned at deletion.
	var errs []error
	for _, c := range b.Collections {
		if id, err := s.DB.CollectionIDByPath(ctx, c.Path); err == nil {
			errs = append(errs, s.DB.SetCollectionTags(ctx, id, c.Tags), s.DB.SetCollectionSourceURL(ctx, id, c.SourceURL))
		}
	}
	for _, m := range b.Models {
		if id, err := s.DB.ModelIDByPath(ctx, m.Path); err == nil {
			errs = append(errs, s.DB.SetModelTags(ctx, id, m.Tags), s.DB.SetModelSourceURL(ctx, id, m.SourceURL))
			if m.Up != "" {
				errs = append(errs, s.DB.SetModelUp(ctx, id, m.Up))
			}
		}
	}
	slog.Info("restored from trash", "batch", id, "items", len(b.Items))
	return b, errors.Join(errs...)
}

// PurgeTrash permanently deletes one batch, or every batch if id is "".
func (s *Scanner) PurgeTrash(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.removed()
	if id == "" {
		return os.RemoveAll(s.trashRoot())
	}
	if _, err := s.readBatch(id); err != nil {
		return err
	}
	return os.RemoveAll(s.batchDir(id))
}

// trashReason describes a deletion for the trash list.
func trashReason(items []DeleteItem) string {
	if len(items) == 1 {
		return "Deleted " + path.Base(items[0].Path)
	}
	return fmt.Sprintf("Deleted %d items", len(items))
}
