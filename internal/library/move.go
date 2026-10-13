package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/denverquane/stilton/internal/db"
)

// InvalidError means the request can't be carried out as asked; the message
// is meant for the user.
type InvalidError struct{ msg string }

func (e *InvalidError) Error() string { return e.msg }

// ConflictError means the destination or a moved name is already taken.
type ConflictError struct{ msg string }

func (e *ConflictError) Error() string { return e.msg }

func invalid(format string, a ...any) error  { return &InvalidError{fmt.Sprintf(format, a...)} }
func conflict(format string, a ...any) error { return &ConflictError{fmt.Sprintf(format, a...)} }

// ValidateName checks a user-supplied file or folder name.
func ValidateName(name string) error {
	switch {
	case name == "":
		return invalid("name is empty")
	case len(name) > 255:
		return invalid("name is too long")
	case name == "." || name == "..":
		return invalid("%q isn't a valid name", name)
	case strings.HasPrefix(name, "."):
		return invalid("names can't start with \".\"; hidden files and folders aren't indexed")
	case strings.ContainsAny(name, "/\\\x00"):
		return invalid("names can't contain / or \\")
	}
	return nil
}

// CreateCollection makes a new folder named name inside parentID and moves
// the given models and collections into it. See MoveItems.
func (s *Scanner) CreateCollection(ctx context.Context, parentID int64, name string, modelIDs, collectionIDs []int64) (db.Collection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return db.Collection{}, err
	}
	parent, err := s.destination(ctx, parentID)
	if err != nil {
		return db.Collection{}, err
	}
	dest := path.Join(parent.Path, name)
	sel, err := s.selection(ctx, modelIDs, collectionIDs, dest)
	if err != nil {
		return db.Collection{}, err
	}

	destAbs := s.abs(dest)
	if _, err := os.Lstat(destAbs); err == nil {
		return db.Collection{}, conflict("%q already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return db.Collection{}, err
	}
	if exists, err := s.DB.CollectionExists(ctx, dest); err != nil {
		return db.Collection{}, err
	} else if exists {
		return db.Collection{}, conflict("%q already exists; rescan if it was just deleted", dest)
	}
	if err := sel.checkNames(nil); err != nil {
		return db.Collection{}, err
	}

	if err := os.Mkdir(destAbs, 0o755); err != nil {
		return db.Collection{}, err
	}
	var id int64
	err = s.moveInto(ctx, dest, sel, func() (err error) {
		id, err = s.DB.CreateAndMoveInto(ctx, parentID, dest, name, sel.models, sel.collections)
		return err
	})
	if err != nil {
		os.Remove(destAbs)
		return db.Collection{}, err
	}
	slog.Info("created collection", "path", dest, "models", len(sel.models), "collections", len(sel.collections))
	return s.DB.GetCollection(ctx, id)
}

// MoveItems moves models and collections into an existing collection, on disk
// and in the index. Index rows are moved rather than recreated, so tags and
// source links are kept. Items already in the destination are skipped.
// Nothing moves if any name would clash with something already there.
// Folders left completely empty by the move are removed.
func (s *Scanner) MoveItems(ctx context.Context, destID int64, modelIDs, collectionIDs []int64) (db.Collection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dest, err := s.destination(ctx, destID)
	if err != nil {
		return db.Collection{}, err
	}
	sel, err := s.selection(ctx, modelIDs, collectionIDs, dest.Path)
	if err != nil {
		return db.Collection{}, err
	}

	// Check against everything in the folder on disk, not just indexed
	// models: a notes file or unindexed subfolder would be overwritten too.
	entries, err := os.ReadDir(s.abs(dest.Path))
	if err != nil {
		return db.Collection{}, err
	}
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		existing[strings.ToLower(e.Name())] = true
	}
	if err := sel.checkNames(existing); err != nil {
		return db.Collection{}, err
	}

	err = s.moveInto(ctx, dest.Path, sel, func() error {
		return s.DB.MoveInto(ctx, dest.ID, dest.Path, sel.models, sel.collections)
	})
	if err != nil {
		return db.Collection{}, err
	}
	slog.Info("moved into collection", "path", dest.Path, "models", len(sel.models), "collections", len(sel.collections))
	return dest, nil
}

func (s *Scanner) destination(ctx context.Context, id int64) (db.Collection, error) {
	c, err := s.DB.GetCollection(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return c, invalid("the destination folder no longer exists; refresh and try again")
	}
	return c, err
}

// moveInto renames the selection into dest (a library-relative folder that
// exists on disk), then runs record to update the index. If any rename or
// record fails, completed renames are undone so the index never points at
// files that aren't there. On success, emptied source folders are removed.
func (s *Scanner) moveInto(ctx context.Context, dest string, sel selection, record func() error) error {
	type move struct{ from, to string }
	var done []move
	undo := func(cause error) error {
		for i := len(done) - 1; i >= 0; i-- {
			if err := os.Rename(done[i].to, done[i].from); err != nil {
				slog.Error("undo move failed; rescan to resync", "from", done[i].to, "to", done[i].from, "err", err)
				return fmt.Errorf("%w (and undoing the move failed; files may be split between folders, so rescan)", cause)
			}
		}
		return cause
	}

	destAbs := s.abs(dest)
	var sources []string
	for _, rel := range sel.paths() {
		from, to := s.abs(rel), filepath.Join(destAbs, path.Base(rel))
		if err := os.Rename(from, to); err != nil {
			return undo(err)
		}
		done = append(done, move{from, to})
		sources = append(sources, filepath.Dir(from))
	}
	if err := record(); err != nil {
		return undo(err)
	}

	for _, dir := range sources {
		s.removeEmptyDirs(dir)
	}
	if _, err := s.DB.PruneCollections(ctx); err != nil {
		return err
	}
	return s.DB.PruneTags(ctx)
}

// selection is what a move will actually relocate.
type selection struct {
	models      []db.Model
	collections []db.Collection
}

func (sel selection) paths() []string {
	var out []string
	for _, c := range sel.collections {
		out = append(out, c.Path)
	}
	for _, m := range sel.models {
		out = append(out, m.Path)
	}
	return out
}

// checkNames ensures every moved item gets a distinct name in the destination,
// which already holds the (lowercased) names in existing. Comparison is
// case-insensitive since libraries often live on case-insensitive shares.
func (sel selection) checkNames(existing map[string]bool) error {
	names := map[string]string{}
	for _, rel := range sel.paths() {
		base := path.Base(rel)
		key := strings.ToLower(base)
		if existing[key] {
			return conflict("the destination already has something named %q", base)
		}
		if other, ok := names[key]; ok {
			return conflict("%q and %q would have the same name in the destination", other, rel)
		}
		names[key] = rel
	}
	return nil
}

// selection loads the models and collections to move into dest, dropping
// items already directly in dest.
func (s *Scanner) selection(ctx context.Context, modelIDs, collectionIDs []int64, dest string) (selection, error) {
	all, err := s.loadSelection(ctx, modelIDs, collectionIDs, "moved")
	if err != nil {
		return selection{}, err
	}
	var sel selection
	for _, c := range all.collections {
		if within(dest, c.Path) {
			return selection{}, invalid("%q can't be moved into itself or a folder inside it", c.Name)
		}
		if dirOf(c.Path) != dest {
			sel.collections = append(sel.collections, c)
		}
	}
	for _, m := range all.models {
		if dirOf(m.Path) != dest {
			sel.models = append(sel.models, m)
		}
	}
	if len(sel.models) == 0 && len(sel.collections) == 0 {
		return selection{}, invalid("everything selected is already in that folder")
	}
	return sel, nil
}

// loadSelection loads selected models and collections. Collections nested in
// another selected collection, and models inside a selected collection, are
// dropped since they're handled along with it. verb ("moved", "deleted")
// words the error for selecting the library root.
func (s *Scanner) loadSelection(ctx context.Context, modelIDs, collectionIDs []int64, verb string) (selection, error) {
	if len(modelIDs) == 0 && len(collectionIDs) == 0 {
		return selection{}, invalid("nothing selected")
	}
	var all []db.Collection
	for _, id := range collectionIDs {
		c, err := s.DB.GetCollection(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			return selection{}, invalid("a selected folder no longer exists; refresh and try again")
		} else if err != nil {
			return selection{}, err
		}
		if c.ParentID == nil {
			return selection{}, invalid("the library root can't be %s", verb)
		}
		all = append(all, c)
	}

	var sel selection
	for _, c := range all {
		nested := false
		for _, other := range all {
			if other.ID != c.ID && within(c.Path, other.Path) {
				nested = true
			}
		}
		if !nested {
			sel.collections = append(sel.collections, c)
		}
	}
	seen := map[int64]bool{}
	for _, id := range modelIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		m, err := s.DB.GetModel(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			return selection{}, invalid("a selected model no longer exists; refresh and try again")
		} else if err != nil {
			return selection{}, err
		}
		inSelected := false
		for _, c := range sel.collections {
			if within(m.Path, c.Path) {
				inSelected = true
			}
		}
		if !inSelected {
			sel.models = append(sel.models, m)
		}
	}
	return sel, nil
}

// within reports whether rel is dir or somewhere beneath it.
func within(rel, dir string) bool {
	return dir == "" || rel == dir || strings.HasPrefix(rel, dir+"/")
}
