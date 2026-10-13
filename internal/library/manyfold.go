package library

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/denverquane/stilton/internal/db"
)

// manyfoldFile is the metadata file Manyfold writes into each model folder.
// See https://manyfold.app/profiles/0.0/datapackage.json.
const manyfoldFile = "datapackage.json"

// ManyfoldImport is what importing one folder's Manyfold metadata would do.
type ManyfoldImport struct {
	CollectionID int64  `json:"collectionId"`
	Path         string `json:"path"`
	Name         string `json:"name"` // the folder's current name
	// Title is Manyfold's display name, offered as a rename when it differs
	// from the folder name. RenameError says why it can't be used, if so.
	Title       string `json:"title"`
	RenameError string `json:"renameError,omitempty"`
	// AddTags are keywords the folder doesn't have yet; SourceURL is set only
	// if the folder has no source link.
	AddTags   []string `json:"addTags"`
	SourceURL string   `json:"sourceUrl,omitempty"`
	// Orient lists models whose up axis Manyfold records differently.
	Orient []ManyfoldOrient `json:"orient"`
}

// ManyfoldOrient sets one model's up axis from Manyfold's resource list.
type ManyfoldOrient struct {
	ModelID int64  `json:"modelId"`
	Path    string `json:"path"`
	Up      string `json:"up"`
}

// Pending reports whether importing would change anything.
func (m ManyfoldImport) Pending() bool {
	return len(m.AddTags) > 0 || m.SourceURL != "" || len(m.Orient) > 0 ||
		(m.Title != "" && m.Title != m.Name && m.RenameError == "")
}

// manyfoldPackage holds the datapackage.json fields STLton uses. Manyfold's
// homepage is its own page for the model, not where it came from, so it's
// ignored; links are the real sources.
type manyfoldPackage struct {
	Title    string            `json:"title"`
	Keywords []string          `json:"keywords"`
	Links    []json.RawMessage `json:"links"`
	// Resources are the folder's files, relative to it. Up is which axis
	// points up, e.g. "+z".
	Resources []struct {
		Path string `json:"path"`
		Up   string `json:"up"`
	} `json:"resources"`
}

// ManyfoldPreview lists folders whose datapackage.json would add something.
func (s *Scanner) ManyfoldPreview(ctx context.Context) ([]ManyfoldImport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.manyfoldImports(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(m ManyfoldImport) bool { return !m.Pending() }), nil
}

// ManyfoldApplyRequest selects which folders to import, and whether to
// rename each to its Manyfold title.
type ManyfoldApplyRequest struct {
	CollectionID int64 `json:"collectionId"`
	Rename       bool  `json:"rename"`
}

// ManyfoldResult reports one folder's import. Tags and source are applied
// even when the rename fails.
type ManyfoldResult struct {
	CollectionID int64  `json:"collectionId"`
	Path         string `json:"path"`
	Error        string `json:"error,omitempty"`
}

// ManyfoldApply imports the requested folders' metadata, recomputed now
// rather than trusted from an earlier preview.
func (s *Scanner) ManyfoldApply(ctx context.Context, reqs []ManyfoldApplyRequest) ([]ManyfoldResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.manyfoldImports(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[int64]ManyfoldImport{}
	for _, m := range all {
		byID[m.CollectionID] = m
	}

	out := []ManyfoldResult{}
	for _, r := range reqs {
		m, ok := byID[r.CollectionID]
		res := ManyfoldResult{CollectionID: r.CollectionID, Path: m.Path}
		if !ok {
			res.Error = "no Manyfold metadata found for this folder"
			out = append(out, res)
			continue
		}
		var errs []error
		if len(m.AddTags) > 0 {
			errs = append(errs, s.DB.BulkTags(ctx, nil, []int64{m.CollectionID}, m.AddTags, nil))
		}
		if m.SourceURL != "" {
			errs = append(errs, s.DB.SetCollectionSourceURL(ctx, m.CollectionID, m.SourceURL))
		}
		for _, o := range m.Orient {
			errs = append(errs, s.DB.SetModelUp(ctx, o.ModelID, o.Up))
		}
		if len(m.Orient) > 0 && s.OnChange != nil {
			s.OnChange() // render the new orientations
		}
		if r.Rename && m.Title != m.Name {
			if m.RenameError != "" {
				errs = append(errs, errors.New(m.RenameError))
			} else {
				errs = append(errs, s.renameCollection(ctx, m.CollectionID, m.Title))
			}
		}
		if err := errors.Join(errs...); err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out, nil
}

// manyfoldImports reads every indexed datapackage.json and works out what
// it would add to its folder.
func (s *Scanner) manyfoldImports(ctx context.Context) ([]ManyfoldImport, error) {
	files, err := s.DB.FilesNamed(ctx, manyfoldFile)
	if err != nil {
		return nil, err
	}
	out := []ManyfoldImport{}
	for _, f := range files {
		pkg, err := readManyfold(s.abs(f.Path))
		if err != nil {
			slog.Warn("skipping unreadable Manyfold metadata", "path", f.Path, "err", err)
			continue
		}
		c, err := s.DB.GetCollection(ctx, f.CollectionID)
		if err != nil {
			return nil, err
		}
		if c.ParentID == nil {
			continue // a datapackage.json at the library root describes nothing STLton can tag
		}
		m := ManyfoldImport{CollectionID: c.ID, Path: c.Path, Name: c.Name, AddTags: []string{}}

		have := map[string]bool{}
		for _, t := range c.Tags {
			have[strings.ToLower(t)] = true
		}
		for _, k := range pkg.Keywords {
			k = strings.TrimSpace(k)
			// "!"-prefixed keywords (e.g. "!new") are Manyfold's own markers.
			if k == "" || strings.HasPrefix(k, "!") || len(k) > 64 || have[strings.ToLower(k)] {
				continue
			}
			have[strings.ToLower(k)] = true
			m.AddTags = append(m.AddTags, k)
		}
		if c.SourceURL == "" {
			m.SourceURL = firstLink(pkg.Links)
		}
		m.Orient = []ManyfoldOrient{}
		for _, r := range pkg.Resources {
			up := strings.ToLower(strings.TrimSpace(r.Up))
			if !db.ValidUp(up) || r.Path == "" || strings.Contains(r.Path, "..") {
				continue
			}
			rel := path.Join(c.Path, r.Path)
			id, err := s.DB.ModelIDByPath(ctx, rel)
			if err != nil {
				continue // not indexed (moved, renamed, or not a file we know)
			}
			model, err := s.DB.GetModel(ctx, id)
			if err != nil || model.Kind != db.KindModel {
				continue
			}
			if effectiveUp(model.Up) != effectiveUp(up) {
				m.Orient = append(m.Orient, ManyfoldOrient{ModelID: id, Path: rel, Up: up})
			}
		}

		if title := strings.TrimSpace(pkg.Title); title != "" && title != c.Name {
			m.Title = title
			if err := ValidateName(title); err != nil {
				m.RenameError = err.Error()
			} else if s.nameTaken(path.Join(dirOf(c.Path), title), c.Path) {
				m.RenameError = "there's already something named \"" + title + "\" here"
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// nameTaken reports whether rel exists on disk as something other than self
// (comparing case-insensitively, as rename does).
func (s *Scanner) nameTaken(rel, self string) bool {
	entries, err := os.ReadDir(s.abs(dirOf(rel)))
	if err != nil {
		return false
	}
	want, own := strings.ToLower(path.Base(rel)), strings.ToLower(path.Base(self))
	for _, e := range entries {
		if n := strings.ToLower(e.Name()); n == want && n != own {
			return true
		}
	}
	return false
}

func readManyfold(p string) (manyfoldPackage, error) {
	var pkg manyfoldPackage
	f, err := os.Open(p)
	if err != nil {
		return pkg, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return pkg, err
	}
	return pkg, json.Unmarshal(data, &pkg)
}

// firstLink returns the first http(s) URL among Manyfold links, which may be
// bare strings or objects with a path, url, or href.
func firstLink(links []json.RawMessage) string {
	for _, raw := range links {
		var candidates []string
		var str string
		if json.Unmarshal(raw, &str) == nil {
			candidates = append(candidates, str)
		} else {
			var obj struct{ Path, URL, Href string }
			if json.Unmarshal(raw, &obj) == nil {
				candidates = append(candidates, obj.Path, obj.URL, obj.Href)
			}
		}
		for _, c := range candidates {
			u, err := url.Parse(strings.TrimSpace(c))
			if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil {
				return u.String()
			}
		}
	}
	return ""
}

// effectiveUp treats an unknown up axis as +Z.
func effectiveUp(up string) string {
	if up == "" {
		return "+z"
	}
	return up
}
