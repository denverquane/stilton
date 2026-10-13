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
