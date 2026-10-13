package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/denverquane/stilton/internal/db"
)

// RenameModel renames a model's file, keeping its extension. name is the new
// name without the extension; a trailing copy of the extension is dropped.
func (s *Scanner) RenameModel(ctx context.Context, id int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, err := s.DB.GetModel(ctx, id)
	if err != nil {
		return err
	}
	ext := path.Ext(m.Path)
	name = strings.TrimSpace(name)
	if strings.EqualFold(path.Ext(name), ext) {
		name = strings.TrimSpace(strings.TrimSuffix(name, path.Ext(name)))
	}
	if name == "" {
		return invalid("name is empty")
	}
	if err := ValidateName(name + ext); err != nil {
		return err
	}
	newPath := path.Join(dirOf(m.Path), name+ext)
	if newPath == m.Path {
		return nil
	}
	display := name
	if m.Kind == db.KindFile {
		display = name + ext // files are listed with their extension
	}
	return s.rename(m.Path, newPath, func() error {
		return s.DB.RenameModel(ctx, id, newPath, display)
	})
}

// RenameCollection renames a collection's folder. Everything inside moves with
// it, keeping tags and source links.
func (s *Scanner) RenameCollection(ctx context.Context, id int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.DB.GetCollection(ctx, id)
	if err != nil {
		return err
	}
	if c.ParentID == nil {
		return invalid("the library root can't be renamed")
	}
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return err
	}
	newPath := path.Join(dirOf(c.Path), name)
	if newPath == c.Path {
		return nil
	}
	return s.rename(c.Path, newPath, func() error {
		return s.DB.RenameCollection(ctx, id, c.Path, newPath, name)
	})
}

// rename moves oldRel to newRel within the same folder and records it with
// record, renaming back if that fails. The new name must not clash with
// anything else in the folder, ignoring case; changing only the case of the
// item's own name is allowed.
func (s *Scanner) rename(oldRel, newRel string, record func() error) error {
	entries, err := os.ReadDir(s.abs(dirOf(oldRel)))
	if err != nil {
		return err
	}
	self, want := strings.ToLower(path.Base(oldRel)), strings.ToLower(path.Base(newRel))
	if want != self {
		for _, e := range entries {
			if strings.ToLower(e.Name()) == want {
				return conflict("there's already something named %q here", e.Name())
			}
		}
	}

	from, to := s.abs(oldRel), s.abs(newRel)
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if err := record(); err != nil {
		if undoErr := os.Rename(to, from); undoErr != nil {
			slog.Error("undo rename failed; rescan to resync", "from", to, "to", from, "err", undoErr)
			return errors.Join(err, errors.New("undoing the rename also failed; rescan"))
		}
		return err
	}
	slog.Info("renamed", "from", oldRel, "to", newRel)
	return nil
}
