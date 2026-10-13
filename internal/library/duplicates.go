package library

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/denverquane/stilton/internal/db"
)

var ErrNotInGroup = errors.New("model is not a copy of this file")

type ResolveResult struct {
	Deleted []string `json:"deleted"`
	// Skipped copies changed on disk since the last scan, so they're no
	// longer duplicates and were left alone.
	Skipped []string `json:"skipped"`
}

// ResolveDuplicates keeps one model among those with the given hash and
// moves the other files to the trash. The kept model inherits the
// deleted copies' tags (and a source link, if it has none). Folders left
// completely empty are removed too.
func (s *Scanner) ResolveDuplicates(ctx context.Context, hash string, keepID int64) (ResolveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := ResolveResult{Deleted: []string{}, Skipped: []string{}}
	copies, err := s.DB.ModelsByHash(ctx, hash)
	if err != nil {
		return res, err
	}
	var keep *db.Model
	for i := range copies {
		if copies[i].ID == keepID {
			keep = &copies[i]
		}
	}
	if keep == nil {
		return res, ErrNotInGroup
	}
	// The kept file must really hold these contents, or we'd delete the only copy.
	if h, err := hashFile(s.abs(keep.Path)); err != nil || h != hash {
		return res, errors.New("the file to keep has changed or is missing on disk; rescan and try again")
	}

	var trash selection
	var items []DeleteItem
	for _, m := range copies {
		if m.ID == keepID {
			continue
		}
		p := s.abs(m.Path)
		h, err := hashFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Already gone; trashing just drops the row.
		case err != nil || h != hash:
			res.Skipped = append(res.Skipped, m.Path)
			continue
		}
		full, err := s.DB.GetModel(ctx, m.ID) // with tags, for the trash manifest
		if err != nil {
			return res, err
		}
		trash.models = append(trash.models, full)
		items = append(items, DeleteItem{Kind: "model", Path: m.Path, Files: 1, Bytes: m.Size})
		res.Deleted = append(res.Deleted, m.Path)
	}
	if len(trash.models) == 0 {
		return res, nil
	}

	ids := make([]int64, len(trash.models))
	for i, m := range trash.models {
		ids[i] = m.ID
	}
	if err := s.DB.MergeMetadata(ctx, keepID, ids); err != nil {
		return res, err
	}
	_, err = s.trashSelection(ctx, trash, items, "Duplicates of "+path.Base(keep.Path))
	slog.Info("resolved duplicates", "kept", keep.Path, "trashed", len(items))
	return res, err
}

func (s *Scanner) abs(rel string) string {
	return filepath.Join(s.Root, filepath.FromSlash(rel))
}

// removeEmptyDirs removes dir and its parents while they're empty, stopping
// at the library root. Folders holding anything else (notes, images) stay.
func (s *Scanner) removeEmptyDirs(dir string) {
	for dir != s.Root && len(dir) > len(s.Root) {
		if err := os.Remove(dir); err != nil {
			return // not empty, or not removable
		}
		dir = filepath.Dir(dir)
	}
}
