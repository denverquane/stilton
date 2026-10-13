package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denverquane/stilton/internal/db"
	"github.com/denverquane/stilton/internal/library"
	"github.com/denverquane/stilton/internal/thumbnail"
)

type testEnv struct {
	t       *testing.T
	srv     *httptest.Server
	scanner *library.Scanner
	db      *db.DB
}

// newEnv indexes a library containing files (contents are irrelevant; the
// scanner doesn't parse meshes) and serves the API over it.
func newEnv(t *testing.T, files ...string) *testEnv {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	scanner := &library.Scanner{Root: root, DB: database}
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB:      database,
		Scanner: scanner,
		Thumbs:  thumbnail.NewGenerator(database, root, filepath.Join(dataDir, "thumbs"), 1),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{t: t, srv: srv, scanner: scanner, db: database}
}

func (e *testEnv) do(method, path, body string, out any) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return res.StatusCode
}

func (e *testEnv) mustDo(method, path, body string, out any) {
	e.t.Helper()
	if code := e.do(method, path, body, out); code != http.StatusOK {
		e.t.Fatalf("%s %s: status %d", method, path, code)
	}
}

type collectionResp struct {
	Collection struct {
		ID        int64    `json:"id"`
		Name      string   `json:"name"`
		Tags      []string `json:"tags"`
		SourceURL string   `json:"sourceUrl"`
	} `json:"collection"`
	Children []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"children"`
	TagMatches bool `json:"tagMatches"`
	InTagged   bool `json:"inTagged"`
}

type modelsResp struct {
	Models []struct {
		ID        int64    `json:"id"`
		Name      string   `json:"name"`
		Tags      []string `json:"tags"`
		SourceURL string   `json:"sourceUrl"`
	} `json:"models"`
	Flat bool `json:"flat"`
}

func (r modelsResp) names() []string {
	var n []string
	for _, m := range r.Models {
		n = append(n, m.Name)
	}
	slices.Sort(n)
	return n
}

func (r collectionResp) childNames() []string {
	var n []string
	for _, c := range r.Children {
		n = append(n, c.Name)
	}
	return n
}

// ids looks up collection and model ids by name.
func (e *testEnv) ids() (collections, models map[string]int64) {
	e.t.Helper()
	collections, models = map[string]int64{}, map[string]int64{}
	var walk func(id string)
	walk = func(id string) {
		var c collectionResp
		e.mustDo("GET", "/api/collections/"+id, "", &c)
		for _, child := range c.Children {
			collections[child.Name] = child.ID
			walk(itoa(child.ID))
		}
	}
	walk("root")
	var all modelsResp
	e.mustDo("GET", "/api/models?q=m&limit=500", "", &all)
	for _, m := range all.Models {
		models[m.Name] = m.ID
	}
	return collections, models
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestTagFilterFolders(t *testing.T) {
	e := newEnv(t, "m5.stl", "A/m1.stl", "A/A2/m2.stl", "B/m3.stl", "B/m4.stl")
	cols, models := e.ids()
	tag := func(path, tags string) { e.mustDo("PATCH", path, `{"tags":`+tags+`}`, nil) }
	tag("/api/collections/"+itoa(cols["A"]), `["T"]`)
	tag("/api/collections/"+itoa(cols["A2"]), `["T"]`)
	tag("/api/models/"+itoa(models["m1"]), `["T"]`)
	tag("/api/models/"+itoa(models["m3"]), `["T"]`)
	tag("/api/models/"+itoa(models["m5"]), `["T"]`)

	tests := []struct {
		name, collection, query   string
		wantChildren, wantModels  []string
		wantTagMatches, wantInTag bool
		wantFlat                  bool
	}{
		// A is shown as a folder; A2 is inside it so it isn't. m1 and m2 are
		// represented by A; m3 and m5 are tagged in untagged folders.
		{"root", "root", "tag=T", []string{"A"}, []string{"m3", "m5"}, true, false, true},
		// Inside a tagged folder everything matches: browse normally.
		{"inside tagged", itoa(cols["A"]), "tag=T", []string{"A2"}, []string{"m1"}, false, true, false},
		{"untagged folder", itoa(cols["B"]), "tag=T", nil, []string{"m3"}, true, false, true},
		// Tag matching is case-insensitive.
		{"lowercase tag", "root", "tag=t", []string{"A"}, []string{"m3", "m5"}, true, false, true},
		// Search stays flat; a tag matches models directly or via their folders.
		{"search+tag", "root", "q=m&tag=T", []string{"A"}, []string{"m1", "m2", "m3", "m5"}, true, false, true},
		{"no filter", "root", "", []string{"A", "B"}, []string{"m5"}, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c collectionResp
			e.mustDo("GET", "/api/collections/"+tt.collection+"?"+tt.query, "", &c)
			if got := c.childNames(); !slices.Equal(got, tt.wantChildren) {
				t.Errorf("children = %v, want %v", got, tt.wantChildren)
			}
			if c.TagMatches != tt.wantTagMatches || c.InTagged != tt.wantInTag {
				t.Errorf("tagMatches=%v inTagged=%v, want %v %v", c.TagMatches, c.InTagged, tt.wantTagMatches, tt.wantInTag)
			}
			var m modelsResp
			e.mustDo("GET", "/api/models?collection="+tt.collection+"&"+tt.query, "", &m)
			if got := m.names(); !slices.Equal(got, tt.wantModels) {
				t.Errorf("models = %v, want %v", got, tt.wantModels)
			}
			if m.Flat != tt.wantFlat {
				t.Errorf("flat = %v, want %v", m.Flat, tt.wantFlat)
			}
		})
	}
}

func TestPatch(t *testing.T) {
	e := newEnv(t, "A/m1.stl", "A/m2.stl")
	cols, models := e.ids()
	model := "/api/models/" + itoa(models["m1"])
	coll := "/api/collections/" + itoa(cols["A"])

	var m struct {
		Tags      []string `json:"tags"`
		SourceURL string   `json:"sourceUrl"`
	}
	e.mustDo("PATCH", model, `{"sourceUrl":" printables.com/model/1 ","tags":["a","b"]}`, &m)
	if m.SourceURL != "https://printables.com/model/1" || !slices.Equal(m.Tags, []string{"a", "b"}) {
		t.Errorf("after patch: %+v", m)
	}
	// Absent fields are left alone; an empty list clears.
	e.mustDo("PATCH", model, `{}`, &m)
	if m.SourceURL == "" || len(m.Tags) != 2 {
		t.Errorf("empty patch changed fields: %+v", m)
	}
	e.mustDo("PATCH", model, `{"tags":[],"sourceUrl":""}`, &m)
	if m.SourceURL != "" || len(m.Tags) != 0 || m.Tags == nil {
		t.Errorf("clearing: %+v", m)
	}

	for _, bad := range []string{`javascript:alert(1)`, `javascript://x`, `ftp://x.com/a`, `https://user:pw@x.com`, `https://`} {
		if code := e.do("PATCH", model, `{"sourceUrl":`+jsonString(bad)+`,"tags":["x"]}`, nil); code != http.StatusBadRequest {
			t.Errorf("sourceUrl %q: status %d, want 400", bad, code)
		}
	}
	// A rejected URL doesn't half-apply the rest of the patch.
	e.mustDo("GET", model, "", &m)
	if len(m.Tags) != 0 {
		t.Errorf("tags applied despite invalid URL: %v", m.Tags)
	}

	var c struct {
		Tags      []string `json:"tags"`
		SourceURL string   `json:"sourceUrl"`
	}
	e.mustDo("PATCH", coll, `{"tags":["kit"],"sourceUrl":"https://example.com/kit"}`, &c)
	if !slices.Equal(c.Tags, []string{"kit"}) || c.SourceURL != "https://example.com/kit" {
		t.Errorf("collection after patch: %+v", c)
	}
	if code := e.do("PATCH", "/api/collections/root", `{"tags":["x"]}`, nil); code != http.StatusBadRequest {
		t.Errorf("patch root: status %d, want 400", code)
	}
	if code := e.do("PATCH", "/api/collections/9999", `{"tags":["x"]}`, nil); code != http.StatusNotFound {
		t.Errorf("patch missing collection: status %d, want 404", code)
	}

	var tags []struct {
		Name        string `json:"name"`
		Models      int    `json:"models"`
		Collections int    `json:"collections"`
	}
	e.mustDo("GET", "/api/tags", "", &tags)
	if len(tags) != 1 || tags[0].Name != "kit" || tags[0].Collections != 1 {
		t.Errorf("tags = %+v, want only kit on one collection", tags)
	}

	// When a tagged folder's files are removed, the folder and its now-unused tag go too.
	os.RemoveAll(filepath.Join(e.scanner.Root, "A", "m1.stl"))
	os.RemoveAll(filepath.Join(e.scanner.Root, "A", "m2.stl"))
	os.WriteFile(filepath.Join(e.scanner.Root, "keep.stl"), []byte("keep"), 0o644)
	if _, err := e.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mustDo("GET", "/api/tags", "", &tags)
	if len(tags) != 0 {
		t.Errorf("tags after removing folder = %+v, want none", tags)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestDuplicates(t *testing.T) {
	e := newEnv(t, "C/y.stl")
	root := e.scanner.Root
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write("A/x.stl", "same")
	write("B/x.stl", "same")
	write("D/deep/x.stl", "same")
	write("E/x.stl", "same")
	write("E/readme.txt", "keep this folder")
	if _, err := e.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	type group struct {
		Hash   string `json:"hash"`
		Models []struct {
			ID     int64  `json:"id"`
			Path   string `json:"path"`
			Copies int    `json:"copies"`
		} `json:"models"`
	}
	var dupes struct {
		Groups []group `json:"groups"`
		Total  int     `json:"total"`
	}
	e.mustDo("GET", "/api/duplicates", "", &dupes)
	if dupes.Total != 1 || len(dupes.Groups) != 1 || len(dupes.Groups[0].Models) != 4 {
		t.Fatalf("duplicates = %+v, want one group of 4", dupes)
	}
	g := dupes.Groups[0]
	ids := map[string]int64{}
	for _, m := range g.Models {
		ids[m.Path] = m.ID
		if m.Copies != 4 {
			t.Errorf("%s: copies = %d, want 4", m.Path, m.Copies)
		}
	}

	e.mustDo("PATCH", "/api/models/"+itoa(ids["A/x.stl"]), `{"tags":["a"]}`, nil)
	e.mustDo("PATCH", "/api/models/"+itoa(ids["D/deep/x.stl"]), `{"tags":["d"],"sourceUrl":"https://example.com/d"}`, nil)
	// A copy edited since the scan is no longer a duplicate and must survive.
	write("E/x.stl", "edited")

	resolve := "/api/duplicates/" + g.Hash + "/resolve"
	var other modelsResp
	e.mustDo("GET", "/api/models?q=y", "", &other)
	if code := e.do("POST", resolve, `{"keepId":`+itoa(other.Models[0].ID)+`}`, nil); code != http.StatusBadRequest {
		t.Errorf("keeping a model from another group: status %d, want 400", code)
	}
	if code := e.do("POST", "/api/duplicates/nothex/resolve", `{"keepId":1}`, nil); code != http.StatusNotFound {
		t.Errorf("bad hash: status %d, want 404", code)
	}

	var res struct {
		Deleted []string `json:"deleted"`
		Skipped []string `json:"skipped"`
	}
	e.mustDo("POST", resolve, `{"keepId":`+itoa(ids["B/x.stl"])+`}`, &res)
	slices.Sort(res.Deleted)
	if !slices.Equal(res.Deleted, []string{"A/x.stl", "D/deep/x.stl"}) || !slices.Equal(res.Skipped, []string{"E/x.stl"}) {
		t.Errorf("deleted %v skipped %v", res.Deleted, res.Skipped)
	}

	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	for rel, want := range map[string]bool{
		"B/x.stl": true, "E/x.stl": true, "E/readme.txt": true,
		"A/x.stl": false, "A": false, "D": false, // emptied folders are removed
	} {
		if exists(rel) != want {
			t.Errorf("%s exists = %v, want %v", rel, !want, want)
		}
	}

	var kept struct {
		Tags      []string `json:"tags"`
		SourceURL string   `json:"sourceUrl"`
		Copies    int      `json:"copies"`
	}
	e.mustDo("GET", "/api/models/"+itoa(ids["B/x.stl"]), "", &kept)
	if !slices.Equal(kept.Tags, []string{"a", "d"}) || kept.SourceURL != "https://example.com/d" {
		t.Errorf("kept model = %+v, want merged tags and source", kept)
	}
	// E/x.stl still has the old hash in the index until a rescan picks up the edit.
	if _, err := e.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mustDo("GET", "/api/duplicates", "", &dupes)
	if dupes.Total != 0 {
		t.Errorf("after resolve and rescan, %d duplicate groups remain", dupes.Total)
	}
	var root2 collectionResp
	e.mustDo("GET", "/api/collections/root", "", &root2)
	if got := root2.childNames(); !slices.Equal(got, []string{"B", "C", "E"}) {
		t.Errorf("collections = %v, want A and D pruned", got)
	}
}

func TestCreateCollection(t *testing.T) {
	e := newEnv(t, "A/x.stl", "A/y.stl", "B/z.stl", "B/sub_1/w.stl", "C/x.STL")
	cols, _ := e.ids()
	root := e.scanner.Root
	e.mustDo("PATCH", "/api/collections/"+itoa(cols["sub_1"]), `{"tags":["kit"],"sourceUrl":"https://example.com/kit"}`, nil)

	var aModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["A"]), "", &aModels)
	var ax, ay int64
	for _, m := range aModels.Models {
		if m.Name == "x" {
			ax = m.ID
		} else {
			ay = m.ID
		}
	}
	e.mustDo("PATCH", "/api/models/"+itoa(ax), `{"tags":["keep"]}`, nil)

	create := func(parent, name string, models, collections []int64) int {
		b, _ := json.Marshal(map[string]any{"parent": parent, "name": name, "models": models, "collections": collections})
		return e.do("POST", "/api/collections", string(b), nil)
	}

	if code := create("root", "Kit", []int64{ax, ay}, []int64{cols["sub_1"]}); code != http.StatusCreated {
		t.Fatalf("create Kit: status %d", code)
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	for rel, want := range map[string]bool{
		"Kit/x.stl": true, "Kit/y.stl": true, "Kit/sub_1/w.stl": true, "B/z.stl": true,
		"A": false, "B/sub_1": false,
	} {
		if exists(rel) != want {
			t.Errorf("%s exists = %v, want %v", rel, !want, want)
		}
	}

	// The index moved with the files: a rescan finds nothing to change.
	res, err := e.scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Added+res.Updated+res.Removed != 0 {
		t.Errorf("rescan after move = %+v, want no changes", res)
	}

	cols, _ = e.ids()
	var kit, sub collectionResp
	e.mustDo("GET", "/api/collections/"+itoa(cols["Kit"]), "", &kit)
	if got := kit.childNames(); !slices.Equal(got, []string{"sub_1"}) {
		t.Errorf("Kit children = %v", got)
	}
	e.mustDo("GET", "/api/collections/"+itoa(cols["sub_1"]), "", &sub)
	if !slices.Equal(sub.Collection.Tags, []string{"kit"}) || sub.Collection.SourceURL != "https://example.com/kit" {
		t.Errorf("moved collection lost its tags or link: %+v", sub.Collection)
	}
	var kitModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["Kit"]), "", &kitModels)
	if got := kitModels.names(); !slices.Equal(got, []string{"x", "y"}) {
		t.Errorf("Kit models = %v", got)
	}
	for _, m := range kitModels.Models {
		if m.Name == "x" && !slices.Equal(m.Tags, []string{"keep"}) {
			t.Errorf("moved model lost its tags: %v", m.Tags)
		}
	}

	var cModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["C"]), "", &cModels)
	cx := cModels.Models[0].ID
	for _, tc := range []struct {
		name, parent, folder string
		models, collections  []int64
		want                 int
	}{
		{"case-insensitive collision", "root", "Other", []int64{cx, ax}, nil, http.StatusConflict},
		{"existing folder", "root", "B", []int64{cx}, nil, http.StatusConflict},
		{"parent traversal", "root", "../x", []int64{cx}, nil, http.StatusBadRequest},
		{"hidden", "root", ".hidden", []int64{cx}, nil, http.StatusBadRequest},
		{"empty name", "root", "  ", []int64{cx}, nil, http.StatusBadRequest},
		{"nothing selected", "root", "Empty", nil, nil, http.StatusBadRequest},
		{"into itself", itoa(cols["Kit"]), "Inner", nil, []int64{cols["Kit"]}, http.StatusBadRequest},
		{"root", "root", "Everything", nil, []int64{db.RootCollectionID}, http.StatusBadRequest},
	} {
		if code := create(tc.parent, tc.folder, tc.models, tc.collections); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
	// Rejected requests moved nothing.
	if !exists("C/x.STL") || !exists("Kit/x.stl") || exists("Other") {
		t.Error("a rejected request changed files on disk")
	}

	// A subcollection: models from two folders gathered inside Kit.
	if code := create(itoa(cols["Kit"]), "Gathered", []int64{cx}, nil); code != http.StatusCreated {
		t.Fatalf("create Kit/Gathered: status %d", code)
	}
	if !exists("Kit/Gathered/x.STL") || exists("C") {
		t.Error("Kit/Gathered not created as expected")
	}
	if res, _ := e.scanner.Scan(context.Background()); res.Added+res.Updated+res.Removed != 0 {
		t.Errorf("rescan after second move = %+v, want no changes", res)
	}
}

func TestMoveIntoExisting(t *testing.T) {
	e := newEnv(t, "A/x.stl", "A/y.stl", "B/z.stl", "B/Y.stl", "C/sub/w.stl", "E/extras/m.stl")
	root := e.scanner.Root
	os.MkdirAll(filepath.Join(root, "B", "extras"), 0o755) // unindexed folder
	os.WriteFile(filepath.Join(root, "B", "extras", "readme.txt"), []byte("hi"), 0o644)
	e.scanner.Scan(context.Background()) // index the extra files so later rescans start clean
	cols, _ := e.ids()

	modelIn := func(collection, name string) int64 {
		var r modelsResp
		e.mustDo("GET", "/api/models?collection="+itoa(cols[collection]), "", &r)
		for _, m := range r.Models {
			if m.Name == name {
				return m.ID
			}
		}
		t.Fatalf("no model %s in %s", name, collection)
		return 0
	}
	ax, ay := modelIn("A", "x"), modelIn("A", "y")
	e.mustDo("PATCH", "/api/models/"+itoa(ax), `{"tags":["keep"]}`, nil)
	e.mustDo("PATCH", "/api/collections/"+itoa(cols["sub"]), `{"tags":["kit"]}`, nil)

	move := func(dest string, models, collections []int64) int {
		b, _ := json.Marshal(map[string]any{"models": models, "collections": collections})
		return e.do("POST", "/api/collections/"+dest+"/move", string(b), nil)
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	rescanClean := func(when string) {
		t.Helper()
		res, err := e.scanner.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Added+res.Updated+res.Removed != 0 {
			t.Errorf("rescan %s = %+v, want no changes", when, res)
		}
	}

	if code := move(itoa(cols["B"]), []int64{ax}, []int64{cols["sub"]}); code != http.StatusOK {
		t.Fatalf("move into B: status %d", code)
	}
	for rel, want := range map[string]bool{"B/x.stl": true, "B/sub/w.stl": true, "A/y.stl": true, "A/x.stl": false, "C": false} {
		if exists(rel) != want {
			t.Errorf("%s exists = %v, want %v", rel, !want, want)
		}
	}
	rescanClean("after move into B")
	cols, _ = e.ids()
	var sub collectionResp
	e.mustDo("GET", "/api/collections/"+itoa(cols["sub"]), "", &sub)
	if !slices.Equal(sub.Collection.Tags, []string{"kit"}) {
		t.Errorf("moved collection tags = %v", sub.Collection.Tags)
	}
	var bModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["B"]), "", &bModels)
	for _, m := range bModels.Models {
		if m.Name == "x" && !slices.Equal(m.Tags, []string{"keep"}) {
			t.Errorf("moved model tags = %v", m.Tags)
		}
	}

	for _, tc := range []struct {
		name, dest          string
		models, collections []int64
		want                int
	}{
		{"collides with indexed model (case-insensitive)", itoa(cols["B"]), []int64{ay}, nil, http.StatusConflict},
		{"collides with unindexed folder", itoa(cols["B"]), nil, []int64{cols["extras"]}, http.StatusConflict},
		{"into itself", itoa(cols["sub"]), nil, []int64{cols["B"]}, http.StatusBadRequest},
		{"already there", itoa(cols["A"]), []int64{ay}, nil, http.StatusBadRequest},
		{"missing destination", "9999", []int64{ay}, nil, http.StatusBadRequest},
	} {
		if code := move(tc.dest, tc.models, tc.collections); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
	if !exists("A/y.stl") || !exists("E/extras/m.stl") || !exists("B/extras/readme.txt") {
		t.Error("a rejected move changed files on disk")
	}

	// Up to the library root.
	if code := move("root", nil, []int64{cols["sub"]}); code != http.StatusOK {
		t.Fatalf("move sub to root: status %d", code)
	}
	if !exists("sub/w.stl") || exists("B/sub") {
		t.Error("sub not moved to root")
	}
	rescanClean("after move to root")
	cols, _ = e.ids()
	var rootView collectionResp
	e.mustDo("GET", "/api/collections/root", "", &rootView)
	if got := rootView.childNames(); !slices.Equal(got, []string{"A", "B", "E", "sub"}) {
		t.Errorf("root children = %v", got)
	}
}

func TestRename(t *testing.T) {
	e := newEnv(t, "A/x.STL", "A/y.stl", "A/sub/w.stl", "B/z.stl")
	root := e.scanner.Root
	os.WriteFile(filepath.Join(root, "A", "notes.txt"), []byte("hi"), 0o644)
	e.scanner.Scan(context.Background())
	cols, _ := e.ids()
	var aModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["A"]), "", &aModels)
	ids := map[string]int64{}
	for _, m := range aModels.Models {
		ids[m.Name] = m.ID
	}
	e.mustDo("PATCH", "/api/models/"+itoa(ids["x"]), `{"tags":["keep"]}`, nil)
	e.mustDo("PATCH", "/api/collections/"+itoa(cols["sub"]), `{"tags":["kit"]}`, nil)

	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	rescanClean := func(when string) {
		t.Helper()
		res, err := e.scanner.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Added+res.Updated+res.Removed != 0 {
			t.Errorf("rescan %s = %+v, want no changes", when, res)
		}
	}

	// The extension is kept as it was; typing it again is harmless.
	var m struct {
		Name string   `json:"name"`
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	e.mustDo("PATCH", "/api/models/"+itoa(ids["x"]), `{"name":" Front Plate.stl "}`, &m)
	if m.Name != "Front Plate" || m.Path != "A/Front Plate.STL" || !exists("A/Front Plate.STL") || exists("A/x.STL") {
		t.Errorf("after model rename: %+v", m)
	}
	if !slices.Equal(m.Tags, []string{"keep"}) {
		t.Errorf("renamed model lost its tags: %v", m.Tags)
	}
	// Changing only the case of its own name is allowed.
	e.mustDo("PATCH", "/api/models/"+itoa(ids["x"]), `{"name":"front plate"}`, &m)
	if m.Path != "A/front plate.STL" {
		t.Errorf("case-only rename: path = %q", m.Path)
	}
	rescanClean("after model renames")

	e.mustDo("PATCH", "/api/collections/"+itoa(cols["sub"]), `{"name":"Parts"}`, nil)
	if !exists("A/Parts/w.stl") || exists("A/sub") {
		t.Error("folder not renamed on disk")
	}
	rescanClean("after folder rename")
	cols, _ = e.ids()
	var parts collectionResp
	e.mustDo("GET", "/api/collections/"+itoa(cols["Parts"]), "", &parts)
	if parts.Collection.Name != "Parts" || !slices.Equal(parts.Collection.Tags, []string{"kit"}) {
		t.Errorf("renamed folder = %+v", parts.Collection)
	}

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"clash with model, any case", "/api/models/" + itoa(ids["y"]), `{"name":"FRONT PLATE"}`, http.StatusConflict},
		{"folder clash with unindexed file", "/api/collections/" + itoa(cols["Parts"]), `{"name":"notes.txt"}`, http.StatusConflict},
		{"empty", "/api/models/" + itoa(ids["y"]), `{"name":" .stl "}`, http.StatusBadRequest},
		{"slash", "/api/collections/" + itoa(cols["Parts"]), `{"name":"a/b"}`, http.StatusBadRequest},
		{"hidden", "/api/collections/" + itoa(cols["Parts"]), `{"name":".parts"}`, http.StatusBadRequest},
		{"root", "/api/collections/root", `{"name":"Everything"}`, http.StatusBadRequest},
	} {
		if code := e.do("PATCH", tc.path, tc.body, nil); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
	// A refused rename doesn't apply the rest of the patch either.
	e.do("PATCH", "/api/models/"+itoa(ids["y"]), `{"name":"front plate","tags":["nope"]}`, nil)
	e.mustDo("GET", "/api/models/"+itoa(ids["y"]), "", &m)
	if m.Name != "y" || len(m.Tags) != 0 {
		t.Errorf("refused rename changed the model: %+v", m)
	}
	if !exists("A/y.stl") || !exists("A/notes.txt") {
		t.Error("a refused rename changed files on disk")
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t, "A/x.stl", "A/y.stl", "B/z.stl", "B/sub/w.stl", "B/sub/deep/v.3mf", "C/u.stl", "D/t.stl")
	root := e.scanner.Root
	write := func(rel string) {
		os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(rel), 0o644)
	}
	write("B/sub/datapackage.json")
	write("B/sub/deep/.DS_Store") // hidden files inside a deleted folder go too
	write("C/readme.txt")
	e.scanner.Scan(context.Background())
	cols, _ := e.ids()
	var bModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["B"]), "", &bModels)
	bz := bModels.Models[0].ID
	var aModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["A"]), "", &aModels)
	ax := aModels.Models[0].ID
	e.mustDo("PATCH", "/api/collections/"+itoa(cols["sub"]), `{"tags":["gone"]}`, nil)

	type plan struct {
		Items []struct {
			Kind  string `json:"kind"`
			Path  string `json:"path"`
			Files int    `json:"files"`
		} `json:"items"`
		Files, ModelFiles, OtherFiles, Folders int
	}
	body := func(models, collections []int64, expect *int) string {
		b, _ := json.Marshal(map[string]any{"models": models, "collections": collections, "expectFiles": expect})
		return string(b)
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}

	// sub is inside B (selected), and so is z: both are covered by B.
	sel := func(expect *int) string { return body([]int64{ax, bz}, []int64{cols["B"], cols["sub"]}, expect) }
	var p plan
	e.mustDo("POST", "/api/delete/preview", sel(nil), &p)
	if p.Files != 6 || p.ModelFiles != 4 || p.OtherFiles != 2 || p.Folders != 3 || len(p.Items) != 2 {
		t.Fatalf("preview = %+v, want 6 files (4 models, 2 other) in 3 folders, 2 items", p)
	}
	if exists("B") != true {
		t.Fatal("preview deleted something")
	}

	// Contents changed since the preview: refuse, delete nothing.
	write("B/new-notes.txt")
	stale := p.Files
	if code := e.do("POST", "/api/delete", sel(&stale), nil); code != http.StatusConflict {
		t.Errorf("stale preview: status %d, want 409", code)
	}
	if code := e.do("POST", "/api/delete", sel(nil), nil); code != http.StatusBadRequest {
		t.Errorf("no expectFiles: status %d, want 400", code)
	}
	if !exists("B/z.stl") || !exists("A/x.stl") {
		t.Fatal("refused delete removed files")
	}

	e.mustDo("POST", "/api/delete/preview", sel(nil), &p)
	fresh := p.Files
	e.mustDo("POST", "/api/delete", sel(&fresh), &p)
	if p.Files != 7 {
		t.Errorf("deleted %d files, want 7", p.Files)
	}
	for rel, want := range map[string]bool{"B": false, "A/x.stl": false, "A/y.stl": true, "C/u.stl": true} {
		if exists(rel) != want {
			t.Errorf("%s exists = %v, want %v", rel, !want, want)
		}
	}
	res, err := e.scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Added+res.Updated+res.Removed != 0 {
		t.Errorf("rescan after delete = %+v, want no changes", res)
	}
	var tags []struct{ Name string }
	e.mustDo("GET", "/api/tags", "", &tags)
	if len(tags) != 0 {
		t.Errorf("tag of deleted folder survived: %+v", tags)
	}

	// Deleting a folder's only model removes the folder only if nothing else is in it.
	cols, _ = e.ids()
	for _, name := range []string{"C", "D"} {
		var m modelsResp
		e.mustDo("GET", "/api/models?collection="+itoa(cols[name]), "", &m)
		one := 1
		e.mustDo("POST", "/api/delete", body([]int64{m.Models[0].ID}, nil, &one), nil)
	}
	if !exists("C/readme.txt") || exists("D") {
		t.Errorf("C kept = %v (want true), D kept = %v (want false)", exists("C"), exists("D"))
	}
	var rootView collectionResp
	e.mustDo("GET", "/api/collections/root", "", &rootView)
	if got := rootView.childNames(); !slices.Equal(got, []string{"A"}) {
		t.Errorf("collections = %v, want just A", got)
	}

	if code := e.do("POST", "/api/delete/preview", body(nil, []int64{db.RootCollectionID}, nil), nil); code != http.StatusBadRequest {
		t.Errorf("deleting root: status %d, want 400", code)
	}
	if code := e.do("POST", "/api/delete/preview", body(nil, nil, nil), nil); code != http.StatusBadRequest {
		t.Errorf("empty selection: status %d, want 400", code)
	}
}

func TestTrash(t *testing.T) {
	e := newEnv(t, "A/x.stl", "A/sub/w.stl", "B/z.stl", "C/u.stl")
	root := e.scanner.Root
	os.WriteFile(filepath.Join(root, "A", "datapackage.json"), []byte("{}"), 0o644)
	cols, _ := e.ids()
	var bModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["B"]), "", &bModels)
	bz := bModels.Models[0].ID
	e.mustDo("PATCH", "/api/collections/"+itoa(cols["sub"]), `{"tags":["kit"],"sourceUrl":"https://example.com/kit"}`, nil)
	e.mustDo("PATCH", "/api/models/"+itoa(bz), `{"tags":["zed"]}`, nil)

	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	rescanClean := func(when string) {
		t.Helper()
		res, err := e.scanner.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Added+res.Updated+res.Removed != 0 {
			t.Errorf("rescan %s = %+v, want no changes", when, res)
		}
	}
	del := func(models, collections []int64) string {
		var p struct {
			Files   int    `json:"files"`
			TrashID string `json:"trashId"`
		}
		b, _ := json.Marshal(map[string]any{"models": models, "collections": collections})
		e.mustDo("POST", "/api/delete/preview", string(b), &p)
		b, _ = json.Marshal(map[string]any{"models": models, "collections": collections, "expectFiles": p.Files})
		e.mustDo("POST", "/api/delete", string(b), &p)
		return p.TrashID
	}

	id := del([]int64{bz}, []int64{cols["A"]})
	if exists("A") || exists("B") {
		t.Fatal("deleted items still in the library")
	}
	for _, rel := range []string{"A/x.stl", "A/sub/w.stl", "A/datapackage.json", "B/z.stl"} {
		if !exists(".stilton-trash/" + id + "/files/" + rel) {
			t.Errorf("%s not in trash batch", rel)
		}
	}
	rescanClean("with items in the trash") // the trash folder isn't indexed

	var trash struct {
		Batches []struct {
			ID     string `json:"id"`
			Files  int    `json:"files"`
			Reason string `json:"reason"`
		} `json:"batches"`
		Files int `json:"files"`
	}
	e.mustDo("GET", "/api/trash", "", &trash)
	if len(trash.Batches) != 1 || trash.Batches[0].ID != id || trash.Files != 4 {
		t.Fatalf("trash = %+v, want one batch of 4 files", trash)
	}

	// Restore is refused if something now occupies an original location.
	os.MkdirAll(filepath.Join(root, "B"), 0o755)
	os.WriteFile(filepath.Join(root, "B", "z.stl"), []byte("new"), 0o644)
	if code := e.do("POST", "/api/trash/"+id+"/restore", "", nil); code != http.StatusConflict {
		t.Errorf("restore over existing file: status %d, want 409", code)
	}
	if exists("A") {
		t.Error("refused restore moved files")
	}
	os.RemoveAll(filepath.Join(root, "B"))
	e.scanner.Scan(context.Background())

	e.mustDo("POST", "/api/trash/"+id+"/restore", "", nil)
	for _, rel := range []string{"A/x.stl", "A/sub/w.stl", "A/datapackage.json", "B/z.stl"} {
		if !exists(rel) {
			t.Errorf("%s not restored", rel)
		}
	}
	if exists(".stilton-trash/" + id) {
		t.Error("restored batch still in trash")
	}
	rescanClean("after restore")
	cols, _ = e.ids()
	var sub collectionResp
	e.mustDo("GET", "/api/collections/"+itoa(cols["sub"]), "", &sub)
	if !slices.Equal(sub.Collection.Tags, []string{"kit"}) || sub.Collection.SourceURL != "https://example.com/kit" {
		t.Errorf("restored folder metadata = %+v", sub.Collection)
	}
	e.mustDo("GET", "/api/models?collection="+itoa(cols["B"]), "", &bModels)
	if len(bModels.Models) != 1 || !slices.Equal(bModels.Models[0].Tags, []string{"zed"}) {
		t.Errorf("restored model = %+v", bModels.Models)
	}

	// Purging one batch, then emptying the trash.
	e.mustDo("GET", "/api/collections/root", "", nil)
	id1 := del(nil, []int64{cols["C"]})
	id2 := del(nil, []int64{cols["B"]})
	if code := e.do("DELETE", "/api/trash/"+id1, "", nil); code != http.StatusNoContent {
		t.Errorf("purge batch: status %d", code)
	}
	if exists(".stilton-trash/"+id1) || !exists(".stilton-trash/"+id2) {
		t.Error("purge removed the wrong batch")
	}
	for _, bad := range []string{"..%2FA", "nope", id1} {
		if code := e.do("POST", "/api/trash/"+bad+"/restore", "", nil); code != http.StatusNotFound {
			t.Errorf("restore %q: status %d, want 404", bad, code)
		}
	}
	if code := e.do("DELETE", "/api/trash", "", nil); code != http.StatusNoContent {
		t.Errorf("empty trash: status %d", code)
	}
	if exists(".stilton-trash") || !exists("A/x.stl") {
		t.Error("emptying the trash removed the wrong things")
	}
}

func TestThumbnailSweep(t *testing.T) {
	e := newEnv(t, "A/keep.stl", "B/trashed.stl")
	dir := t.TempDir()
	g := thumbnail.NewGenerator(e.db, e.scanner.Root, dir, 1)
	hashes, _ := e.db.AllHashes(context.Background())
	var keep, trashed string
	for h := range hashes {
		models, _ := e.db.ModelsByHash(context.Background(), h)
		if strings.HasPrefix(models[0].Path, "A/") {
			keep = h
		} else {
			trashed = h
		}
	}
	orphan := strings.Repeat("ab", 32)
	for _, name := range []string{keep + ".png", trashed + ".png", orphan + ".png", ".thumb-123"} {
		os.WriteFile(filepath.Join(dir, name), []byte("png"), 0o644)
	}

	cols, _ := e.ids()
	b, _ := json.Marshal(map[string]any{"collections": []int64{cols["B"]}, "expectFiles": 1})
	e.mustDo("POST", "/api/delete", string(b), nil)

	keepFn := func(ctx context.Context) (map[string]bool, error) {
		k, _ := e.db.AllHashes(ctx)
		t, _ := e.scanner.TrashHashes()
		for h := range t {
			k[h] = true
		}
		return k, nil
	}
	n, err := g.Sweep(context.Background(), keepFn)
	if err != nil || n != 1 {
		t.Fatalf("sweep removed %d (%v), want 1", n, err)
	}
	for name, want := range map[string]bool{keep + ".png": true, trashed + ".png": true, orphan + ".png": false, ".thumb-123": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", name, err == nil, want)
		}
	}

	// Once the trash is emptied, its thumbnails go on the next sweep.
	if code := e.do("DELETE", "/api/trash", "", nil); code != http.StatusNoContent {
		t.Fatalf("empty trash: status %d", code)
	}
	if n, _ := g.Sweep(context.Background(), keepFn); n != 1 {
		t.Errorf("after emptying trash, sweep removed %d, want 1", n)
	}
}

func TestOtherFiles(t *testing.T) {
	e := newEnv(t, "A/x.stl", "A/datapackage.json", "A/photo.png", "B/datapackage.json", "Docs/manual.pdf", "C/y.stl", "C/copy.json")
	// Identical non-model files aren't duplicates.
	var dupes struct{ Total int }
	e.mustDo("GET", "/api/duplicates", "", &dupes)
	os.WriteFile(filepath.Join(e.scanner.Root, "C", "copy.json"), []byte("A/datapackage.json"), 0o644)
	e.scanner.Scan(context.Background())
	e.mustDo("GET", "/api/duplicates", "", &dupes)
	if dupes.Total != 0 {
		t.Errorf("identical json files reported as %d duplicate sets", dupes.Total)
	}

	var root collectionResp
	e.mustDo("GET", "/api/collections/root", "", &root)
	if got := root.childNames(); !slices.Equal(got, []string{"A", "C"}) {
		t.Errorf("default folders = %v, want file-only folders hidden", got)
	}
	e.mustDo("GET", "/api/collections/root?files=1", "", &root)
	if got := root.childNames(); !slices.Equal(got, []string{"A", "B", "C", "Docs"}) {
		t.Errorf("folders with files = %v", got)
	}

	cols, _ := e.ids()
	var plain, all struct {
		Models []struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			ThumbState string `json:"thumbState"`
		} `json:"models"`
	}
	e.mustDo("GET", "/api/models?collection="+itoa(cols["A"]), "", &plain)
	e.mustDo("GET", "/api/models?collection="+itoa(cols["A"])+"&files=1", "", &all)
	if len(plain.Models) != 1 || plain.Models[0].Kind != "model" {
		t.Errorf("default listing = %+v, want just the model", plain.Models)
	}
	var got []string
	for _, m := range all.Models {
		got = append(got, m.Name+":"+m.Kind+":"+m.ThumbState)
	}
	// Models first; files keep their extension; images get thumbnails, others don't.
	if want := []string{"x:model:pending", "datapackage.json:file:none", "photo.png:file:pending"}; !slices.Equal(got, want) {
		t.Errorf("listing with files = %v, want %v", got, want)
	}
}

func TestDownloads(t *testing.T) {
	e := newEnv(t, "A/x.stl", "A/y.stl", "A/sub/z.stl", "B/w.stl")
	root := e.scanner.Root
	os.WriteFile(filepath.Join(root, "A", "readme.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(root, "A", ".DS_Store"), []byte("junk"), 0o644)
	cols, _ := e.ids()
	var aModels modelsResp
	e.mustDo("GET", "/api/models?collection="+itoa(cols["A"]), "", &aModels)
	ids := map[string]int64{}
	for _, m := range aModels.Models {
		ids[m.Name] = m.ID
	}

	get := func(path string, header ...string) *http.Response {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	get("/api/models/" + itoa(ids["x"]) + "/file")                            // the viewer loading it
	get("/api/models/"+itoa(ids["y"])+"/file?download", "Range", "bytes=10-") // a resumed download
	get("/api/models/" + itoa(ids["y"]) + "/file?download")                   // a real download
	time.Sleep(1100 * time.Millisecond)                                       // timestamps are in seconds
	get("/api/models/" + itoa(ids["x"]) + "/file?download")

	var recent []struct {
		Kind    string `json:"kind"`
		ModelID *int64 `json:"modelId"`
		Path    string `json:"path"`
	}
	e.mustDo("GET", "/api/downloads", "", &recent)
	if len(recent) != 2 || recent[0].Path != "A/x.stl" || recent[1].Path != "A/y.stl" {
		t.Fatalf("downloads = %+v, want x then y, and no viewer loads or resumes", recent)
	}

	var byDownload modelsResp
	e.mustDo("GET", "/api/models?collection=root&q=stl&sort=downloaded", "", &byDownload)
	var order []string
	for _, m := range byDownload.Models {
		order = append(order, m.Name)
	}
	if want := []string{"x", "y", "w", "z"}; !slices.Equal(order, want) {
		t.Errorf("sorted by download = %v, want %v", order, want)
	}

	// Folder download: a zip of everything in it except hidden files.
	res, err := http.Get(e.srv.URL + "/api/collections/" + itoa(cols["A"]) + "/download")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename=A.zip`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	if want := []string{"A/readme.txt", "A/sub/z.stl", "A/x.stl", "A/y.stl"}; !slices.Equal(names, want) {
		t.Errorf("zip entries = %v, want %v", names, want)
	}
	var a collectionResp
	e.mustDo("GET", "/api/collections/"+itoa(cols["A"]), "", &a)
	var stats struct {
		Collection struct {
			DownloadCount    int    `json:"downloadCount"`
			LastDownloadedAt *int64 `json:"lastDownloadedAt"`
		} `json:"collection"`
	}
	e.mustDo("GET", "/api/collections/"+itoa(cols["A"]), "", &stats)
	if stats.Collection.DownloadCount != 1 || stats.Collection.LastDownloadedAt == nil {
		t.Errorf("folder download stats = %+v", stats.Collection)
	}
	if code := e.do("GET", "/api/collections/root/download", "", nil); code != http.StatusBadRequest {
		t.Errorf("root download: status %d, want 400", code)
	}

	// History outlives the item: deleting x keeps its downloads, unlinked.
	one := 1
	b, _ := json.Marshal(map[string]any{"models": []int64{ids["x"]}, "expectFiles": one})
	e.mustDo("POST", "/api/delete", string(b), nil)
	e.mustDo("GET", "/api/downloads", "", &recent)
	if len(recent) != 3 || recent[1].Path != "A/x.stl" || recent[1].ModelID != nil {
		t.Errorf("after deleting x, downloads = %+v, want x's row kept with no model id", recent)
	}
}
