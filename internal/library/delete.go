package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// DeletePlan describes what deleting a selection removes from disk. Folders
// are deleted with everything in them, including files stilton doesn't index.
type DeletePlan struct {
	Items      []DeleteItem `json:"items"`
	Files      int          `json:"files"`      // every file, ModelFiles + OtherFiles
	ModelFiles int          `json:"modelFiles"` // .stl, .obj, .3mf
	OtherFiles int          `json:"otherFiles"` // anything else, e.g. datapackage.json
	Folders    int          `json:"folders"`    // selected folders and the folders inside them
	Bytes      int64        `json:"bytes"`
	// TrashID is the trash batch the items were moved into, once deleted.
	TrashID string `json:"trashId,omitempty"`
}

// DeleteItem is one selected file or folder in a DeletePlan.
type DeleteItem struct {
	Kind  string `json:"kind"` // "model" or "collection"
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// PlanDelete reports what deleting the selection would remove, without
// deleting anything.
func (s *Scanner) PlanDelete(ctx context.Context, modelIDs, collectionIDs []int64) (DeletePlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, plan, err := s.planDelete(ctx, modelIDs, collectionIDs)
	return plan, err
}

// Delete moves the selected model files and folders to the trash and
// removes them from the index. expectFiles must match the plan's file count, so nothing is deleted
// if the folders' contents changed since the user reviewed the plan.
func (s *Scanner) Delete(ctx context.Context, modelIDs, collectionIDs []int64, expectFiles int) (DeletePlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sel, plan, err := s.planDelete(ctx, modelIDs, collectionIDs)
	if err != nil {
		return plan, err
	}
	if plan.Files != expectFiles {
		return plan, conflict("the selection now contains %d files rather than %d; review it again", plan.Files, expectFiles)
	}

	b, err := s.trashSelection(ctx, sel, plan.Items, trashReason(plan.Items))
	plan.TrashID = b.ID
	return plan, err
}

func (s *Scanner) planDelete(ctx context.Context, modelIDs, collectionIDs []int64) (selection, DeletePlan, error) {
	plan := DeletePlan{Items: []DeleteItem{}}
	sel, err := s.loadSelection(ctx, modelIDs, collectionIDs, "deleted")
	if err != nil {
		return sel, plan, err
	}
	for _, c := range sel.collections {
		item := DeleteItem{Kind: "collection", Path: c.Path}
		err := filepath.WalkDir(s.abs(c.Path), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				plan.Folders++
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			item.Files++
			item.Bytes += info.Size()
			if FormatOf(d.Name()) != "" {
				plan.ModelFiles++
			} else {
				plan.OtherFiles++
			}
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			err = nil // already gone; deleting just drops the index rows
		}
		if err != nil {
			return sel, plan, err
		}
		plan.Items = append(plan.Items, item)
		plan.Files += item.Files
		plan.Bytes += item.Bytes
	}
	for _, m := range sel.models {
		item := DeleteItem{Kind: "model", Path: m.Path}
		if info, err := os.Stat(s.abs(m.Path)); err == nil {
			item.Files, item.Bytes = 1, info.Size()
			plan.ModelFiles++
		} else if !errors.Is(err, fs.ErrNotExist) {
			return sel, plan, err
		}
		plan.Items = append(plan.Items, item)
		plan.Files += item.Files
		plan.Bytes += item.Bytes
	}
	return sel, plan, nil
}
