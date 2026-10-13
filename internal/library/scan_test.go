package library

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/denverquane/stilton/internal/db"
)

func writeFiles(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		// Contents only need to be distinct; the scanner doesn't parse meshes.
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newScanner(t *testing.T) *Scanner {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return &Scanner{Root: t.TempDir(), DB: database}
}

// collectionPaths returns every collection path in the tree, depth-first.
func collectionPaths(t *testing.T, d *db.DB, id int64) []string {
	t.Helper()
	children, err := d.ChildCollections(context.Background(), id, db.CollectionOpts{Previews: 4})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range children {
		paths = append(paths, c.Path)
		paths = append(paths, collectionPaths(t, d, c.ID)...)
	}
	return paths
}

func TestScanCollections(t *testing.T) {
	ctx := context.Background()
	s := newScanner(t)
	writeFiles(t, s.Root, "root.stl", "a/x.stl", "a/b/c/y.STL", "a/notes.txt", "d/z.3mf")
	os.MkdirAll(filepath.Join(s.Root, "a", "empty"), 0o755)

	if _, err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	got := collectionPaths(t, s.DB, db.RootCollectionID)
	if want := []string{"a", "a/b", "a/b/c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("collections = %v, want %v", got, want)
	}

	// Counts cover the whole subtree.
	children, _ := s.DB.ChildCollections(ctx, db.RootCollectionID, db.CollectionOpts{Previews: 4})
	if children[0].Name != "a" || children[0].ModelCount != 2 {
		t.Errorf("a: name %q count %d, want a with 2 models", children[0].Name, children[0].ModelCount)
	}

	// Direct vs recursive listing.
	a := children[0].ID
	direct, _, _ := s.DB.ListModels(ctx, db.ListQuery{CollectionID: a, Limit: 100})
	all, _, _ := s.DB.ListModels(ctx, db.ListQuery{CollectionID: a, Recursive: true, Limit: 100})
	if len(direct) != 1 || len(all) != 2 {
		t.Errorf("a: %d direct and %d recursive models, want 1 and 2", len(direct), len(all))
	}

	i := slices.IndexFunc(all, func(m db.Model) bool { return m.Name == "y" })
	chain, err := s.DB.CollectionPath(ctx, all[i].CollectionID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range chain {
		names = append(names, c.Name)
	}
	if want := []string{"Library", "a", "b", "c"}; !slices.Equal(names, want) {
		t.Errorf("path to y.STL = %v, want %v", names, want)
	}

	// Removing the only model under a/b prunes a/b and a/b/c but keeps a.
	os.Remove(filepath.Join(s.Root, "a", "b", "c", "y.STL"))
	if _, err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	got = collectionPaths(t, s.DB, db.RootCollectionID)
	if want := []string{"a", "d"}; !slices.Equal(got, want) {
		t.Fatalf("after delete, collections = %v, want %v", got, want)
	}

	// Re-adding a file in an existing folder and rescanning unchanged files works.
	writeFiles(t, s.Root, "a/w.stl")
	for range 2 {
		if _, err := s.Scan(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got = collectionPaths(t, s.DB, db.RootCollectionID)
	if want := []string{"a", "d"}; !slices.Equal(got, want) {
		t.Fatalf("after rescans, collections = %v, want %v", got, want)
	}
}

func TestScanEmptyLibraryKeepsIndex(t *testing.T) {
	ctx := context.Background()
	s := newScanner(t)
	writeFiles(t, s.Root, "a/b/x.stl")
	if _, err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	// An unmounted drive looks like an empty folder.
	os.RemoveAll(filepath.Join(s.Root, "a"))
	if _, err := s.Scan(ctx); err == nil {
		t.Fatal("expected scanning an empty library with indexed models to fail")
	}
	if got := collectionPaths(t, s.DB, db.RootCollectionID); !slices.Equal(got, []string{"a", "a/b"}) {
		t.Errorf("collections = %v, want them kept", got)
	}
}

func TestScanDetectsMoves(t *testing.T) {
	ctx := context.Background()
	s := newScanner(t)
	writeFiles(t, s.Root,
		"a/x.stl",                  // moved alone
		"kit/p1.stl", "kit/p2.stl", // folder renamed
		"top/s1/q.stl", "top/s2/r.stl", // folder with only subfolders renamed
		"half/h1.stl", "half/h2.stl", // half moved out
		"old/o.stl", "shelf/existing.stl", // moved into an existing folder
		"copy/c.stl", "edit/e.stl",
	)
	// Identical files: one of the pair moves.
	os.MkdirAll(filepath.Join(s.Root, "dup1"), 0o755)
	os.MkdirAll(filepath.Join(s.Root, "dup2"), 0o755)
	os.WriteFile(filepath.Join(s.Root, "dup1", "d.stl"), []byte("same"), 0o644)
	os.WriteFile(filepath.Join(s.Root, "dup2", "d.stl"), []byte("same"), 0o644)
	if _, err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	modelID := func(p string) int64 {
		t.Helper()
		id, err := s.DB.ModelIDByPath(ctx, p)
		if err != nil {
			t.Fatalf("model %s: %v", p, err)
		}
		return id
	}
	collID := func(p string) int64 {
		t.Helper()
		id, err := s.DB.CollectionIDByPath(ctx, p)
		if err != nil {
			t.Fatalf("collection %s: %v", p, err)
		}
		return id
	}
	x := modelID("a/x.stl")
	s.DB.SetModelTags(ctx, x, []string{"keep"})
	s.DB.RecordDownload(ctx, db.Download{Kind: "model", ModelID: &x, Path: "a/x.stl"})
	kit, top, half, old, shelf := collID("kit"), collID("top"), collID("half"), collID("old"), collID("shelf")
	s.DB.SetCollectionTags(ctx, kit, []string{"kit"})
	s.DB.SetCollectionSourceURL(ctx, kit, "https://example.com/kit")
	s.DB.RecordDownload(ctx, db.Download{Kind: "collection", CollectionID: &kit, Path: "kit"})
	s.DB.SetCollectionTags(ctx, top, []string{"top"})
	s.DB.SetCollectionTags(ctx, half, []string{"half"})
	s.DB.SetCollectionTags(ctx, old, []string{"old"})
	s.DB.SetCollectionTags(ctx, shelf, []string{"shelf"})

	mv := func(from, to string) {
		t.Helper()
		dst := filepath.Join(s.Root, filepath.FromSlash(to))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.Rename(filepath.Join(s.Root, filepath.FromSlash(from)), dst); err != nil {
			t.Fatal(err)
		}
	}
	mv("a/x.stl", "b/x.stl")
	mv("kit", "kit renamed")
	mv("top", "archive/top2")
	mv("half/h1.stl", "elsewhere/h1.stl")
	mv("old/o.stl", "shelf/o.stl")
	mv("dup1/d.stl", "dup3/d.stl")
	writeFiles(t, s.Root, "copy2/c.stl") // writeFiles content is the path, so make a true copy:
	data, _ := os.ReadFile(filepath.Join(s.Root, "copy", "c.stl"))
	os.WriteFile(filepath.Join(s.Root, "copy2", "c.stl"), data, 0o644)
	mv("edit/e.stl", "edited/e.stl")
	os.WriteFile(filepath.Join(s.Root, "edited", "e.stl"), []byte("changed"), 0o644)

	res, err := s.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Moved: x, p1, p2, q, r, h1, o, d. Added: the copy and the edited file.
	// Removed: the edited file's old row.
	if want := (ScanResult{Moved: 8, Added: 2, Removed: 1}); res != want {
		t.Errorf("scan = %+v, want %+v", res, want)
	}

	if id := modelID("b/x.stl"); id != x {
		t.Errorf("moved model got a new id (%d, was %d)", id, x)
	}
	m, _ := s.DB.GetModel(ctx, x)
	if !slices.Equal(m.Tags, []string{"keep"}) || m.DownloadCount != 1 {
		t.Errorf("moved model lost metadata: tags %v, downloads %d", m.Tags, m.DownloadCount)
	}

	check := func(path string, tags []string, source string, downloads int) {
		t.Helper()
		c, err := s.DB.GetCollection(ctx, collID(path))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(c.Tags, tags) || c.SourceURL != source || c.DownloadCount != downloads {
			t.Errorf("%s: tags %v source %q downloads %d; want %v %q %d", path, c.Tags, c.SourceURL, c.DownloadCount, tags, source, downloads)
		}
	}
	check("kit renamed", []string{"kit"}, "https://example.com/kit", 1)
	check("archive/top2", []string{"top"}, "", 0)
	check("archive", []string{}, "", 0) // the new parent isn't the moved folder
	check("half", []string{"half"}, "", 0)
	check("elsewhere", []string{}, "", 0)    // half didn't move as a whole
	check("shelf", []string{"shelf"}, "", 0) // existing folder keeps only its own tags

	if _, err := s.DB.CollectionIDByPath(ctx, "kit"); err == nil {
		t.Error("old folder kit still indexed")
	}
	if _, err := s.DB.ModelIDByPath(ctx, "dup2/d.stl"); err != nil {
		t.Error("the unmoved duplicate lost its row")
	}

	// And the index matches disk.
	if res, _ := s.Scan(ctx); res != (ScanResult{}) {
		t.Errorf("rescan = %+v, want no changes", res)
	}
}
