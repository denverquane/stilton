// Package server exposes the JSON API, model files, thumbnails, and the web UI.
package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/denverquane/stilton/internal/db"
	"github.com/denverquane/stilton/internal/library"
	"github.com/denverquane/stilton/internal/mesh"
	"github.com/denverquane/stilton/internal/thumbnail"
)

// Go's mime table doesn't know model formats.
var contentTypes = map[string]string{
	"stl": "model/stl",
	"obj": "model/obj",
	"3mf": "model/3mf",
}

type Server struct {
	DB      *db.DB
	Scanner *library.Scanner
	Thumbs  *thumbnail.Generator
	Web     fs.FS // built frontend; nil or missing index.html serves a placeholder
}

// meshSlots bounds concurrent mesh merges; a multi-million-triangle model can
// take a GB of memory while loaded.
var meshSlots = make(chan struct{}, 2)

// modelJSON is a model as the API returns it, with a ready-to-use thumbnail URL.
type modelJSON struct {
	db.Model
	Thumbnail string `json:"thumbnail,omitempty"`
}

func toJSON(m db.Model) modelJSON {
	out := modelJSON{Model: m}
	if m.ThumbState == db.ThumbOK {
		out.Thumbnail = thumbURL(m.Hash)
	}
	return out
}

func thumbURL(hash string) string { return "/thumbs/" + hash + ".png" }

// collectionPreviews is how many thumbnails a folder card shows.
const collectionPreviews = 4

type collectionChildJSON struct {
	db.CollectionSummary
	Previews []string `json:"previews"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", s.listModels)
	mux.HandleFunc("GET /api/models/{id}", s.getModel)
	mux.HandleFunc("GET /api/models/{id}/file", s.modelFile)
	mux.HandleFunc("GET /api/models/{id}/mesh.stl", s.modelMesh)
	mux.HandleFunc("PATCH /api/models/{id}", s.patchModel)
	mux.HandleFunc("POST /api/collections", s.createCollection)
	mux.HandleFunc("GET /api/collections/{id}", s.getCollection)
	mux.HandleFunc("POST /api/collections/{id}/move", s.moveIntoCollection)
	mux.HandleFunc("GET /api/collections/{id}/download", s.downloadCollection)
	mux.HandleFunc("GET /api/downloads", s.listDownloads)
	mux.HandleFunc("PATCH /api/collections/{id}", s.patchCollection)
	mux.HandleFunc("POST /api/delete/preview", s.previewDelete)
	mux.HandleFunc("POST /api/delete", s.deleteItems)
	mux.HandleFunc("GET /api/trash", s.listTrash)
	mux.HandleFunc("POST /api/trash/{id}/restore", s.restoreTrash)
	mux.HandleFunc("DELETE /api/trash/{id}", s.purgeTrash)
	mux.HandleFunc("DELETE /api/trash", s.purgeTrash)
	mux.HandleFunc("GET /api/duplicates", s.listDuplicates)
	mux.HandleFunc("POST /api/duplicates/{hash}/resolve", s.resolveDuplicates)
	mux.HandleFunc("GET /api/tags", s.listTags)
	mux.HandleFunc("POST /api/rescan", s.rescan)
	mux.HandleFunc("GET /thumbs/{file}", s.thumb)
	mux.Handle("/", s.webHandler())
	return mux
}

// listModels lists models in a collection. What's included depends on the
// filters, mirroring getCollection:
//
//   - no filters: models directly in the collection
//   - q: matching models anywhere beneath it ("flat"); with tag, a model
//     matches if it or any folder above it carries the tag
//   - tag only: if the collection (or an ancestor) carries the tag, everything
//     here matches, so it lists normally. Otherwise it lists tagged models from
//     beneath it, flat, except those inside tagged folders, which getCollection
//     returns as folders instead.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := db.ListQuery{
		IncludeFiles: truthy(q.Get("files")),
		Sort:         q.Get("sort"),
		Limit:        clamp(atoiDefault(q.Get("limit"), 100), 1, 500),
		Offset:       max(atoiDefault(q.Get("offset"), 0), 0),
	}
	if c := q.Get("collection"); c != "" {
		var ok bool
		if lq.CollectionID, ok = parseCollectionID(c); !ok {
			http.Error(w, "invalid collection", http.StatusBadRequest)
			return
		}
	}
	search, tag := strings.TrimSpace(q.Get("q")), strings.TrimSpace(q.Get("tag"))
	switch {
	case search != "":
		lq.Search, lq.Tag, lq.Recursive = search, tag, true
	case tag != "":
		inTagged, err := s.inTagged(r, lq.CollectionID, tag)
		if err != nil {
			serverError(w, err)
			return
		}
		if !inTagged {
			lq.Tag, lq.Recursive, lq.ExcludeTagged = tag, true, true
		}
	}

	models, total, err := s.DB.ListModels(r.Context(), lq)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]modelJSON, len(models))
	for i, m := range models {
		out[i] = toJSON(m)
	}
	writeJSON(w, map[string]any{"models": out, "total": total, "flat": lq.Recursive})
}

// inTagged reports whether a tag filter is already satisfied by the collection
// itself, i.e. it or an ancestor carries the tag. Both listing endpoints use
// this so folders and models agree on the view.
func (s *Server) inTagged(r *http.Request, collectionID int64, tag string) (bool, error) {
	if collectionID == 0 {
		return false, nil
	}
	return s.DB.InTaggedCollection(r.Context(), collectionID, tag)
}

// getCollection returns a collection, its ancestors (for breadcrumbs), and the
// collections to show inside it with counts and preview thumbnails. Normally
// those are its children; with ?tag= (unless the collection is itself inside
// a tagged one) they're the outermost tagged collections anywhere beneath it.
// Models are listed separately via /api/models so they can be paged.
func (s *Server) getCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := parseCollectionID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	chain, err := s.DB.CollectionPath(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}

	q := r.URL.Query()
	tag := strings.TrimSpace(q.Get("tag"))
	opts := db.CollectionOpts{Previews: collectionPreviews, IncludeFiles: truthy(q.Get("files")), Sort: q.Get("sort")}
	inTagged := false
	if tag != "" {
		if inTagged, err = s.inTagged(r, id, tag); err != nil {
			serverError(w, err)
			return
		}
	}
	tagMatches := tag != "" && !inTagged
	var children []db.CollectionSummary
	if tagMatches {
		children, err = s.DB.TaggedCollections(r.Context(), id, tag, opts)
	} else {
		children, err = s.DB.ChildCollections(r.Context(), id, opts)
	}
	if err != nil {
		serverError(w, err)
		return
	}

	out := make([]collectionChildJSON, len(children))
	for i, c := range children {
		out[i] = collectionChildJSON{CollectionSummary: c, Previews: []string{}}
		for _, h := range c.PreviewHashes {
			out[i].Previews = append(out[i].Previews, thumbURL(h))
		}
	}
	writeJSON(w, map[string]any{
		"collection":  chain[len(chain)-1],
		"breadcrumbs": chain,
		"children":    out,
		// Children are tag matches from anywhere beneath, not direct children.
		"tagMatches": tagMatches,
		// The collection or an ancestor carries the tag, so everything here matches.
		"inTagged": inTagged,
	})
}

// createCollection makes a new folder and moves the selected models and
// collections into it, on disk.
func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Parent      json.RawMessage `json:"parent"` // collection id or "root"
		Name        string          `json:"name"`
		Models      []int64         `json:"models"`
		Collections []int64         `json:"collections"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	parentID, ok := parseCollectionID(strings.Trim(string(body.Parent), `"`))
	if !ok {
		http.Error(w, `parent must be a collection id or "root"`, http.StatusBadRequest)
		return
	}
	c, err := s.Scanner.CreateCollection(r.Context(), parentID, body.Name, body.Models, body.Collections)
	writeMoveResult(w, c, err, http.StatusCreated)
}

// moveIntoCollection moves the selected models and collections into an
// existing collection, on disk.
func (s *Server) moveIntoCollection(w http.ResponseWriter, r *http.Request) {
	destID, ok := parseCollectionID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Models      []int64 `json:"models"`
		Collections []int64 `json:"collections"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	c, err := s.Scanner.MoveItems(r.Context(), destID, body.Models, body.Collections)
	writeMoveResult(w, c, err, http.StatusOK)
}

// writeMoveResult responds with the destination collection, or the reason a
// move was refused.
func writeMoveResult(w http.ResponseWriter, c db.Collection, err error, okStatus int) {
	var invalid *library.InvalidError
	var conflict *library.ConflictError
	switch {
	case errors.As(err, &invalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &conflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		slog.Error("move", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(okStatus)
		writeJSON(w, c)
	}
}

// patchBody is a partial update; absent fields are left unchanged.
type patchBody struct {
	// Name renames the file (keeping its extension) or folder on disk.
	Name      *string   `json:"name"`
	Tags      *[]string `json:"tags"`
	SourceURL *string   `json:"sourceUrl"`
}

// decodePatch reads and validates a patchBody, normalizing tags and the URL.
// It writes a 400 and returns false on bad input.
func decodePatch(w http.ResponseWriter, r *http.Request) (patchBody, bool) {
	var body patchBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return body, false
	}
	if body.Tags != nil {
		tags := normalizeTags(*body.Tags)
		body.Tags = &tags
	}
	if body.SourceURL != nil {
		u, err := normalizeSourceURL(*body.SourceURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return body, false
		}
		body.SourceURL = &u
	}
	return body, true
}

func (s *Server) patchModel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, ok := decodePatch(w, r)
	if !ok {
		return
	}
	// Rename first: it's the step most likely to be refused, and nothing
	// else should change if it is.
	if body.Name != nil {
		err = s.Scanner.RenameModel(r.Context(), id, *body.Name)
	}
	if err == nil && body.Tags != nil {
		err = s.DB.SetModelTags(r.Context(), id, *body.Tags)
	}
	if err == nil && body.SourceURL != nil {
		err = s.DB.SetModelSourceURL(r.Context(), id, *body.SourceURL)
	}
	if !s.patchError(w, r, err) {
		s.getModel(w, r)
	}
}

func (s *Server) patchCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := parseCollectionID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if id == db.RootCollectionID {
		// A tag on the root would match the entire library.
		http.Error(w, "the library root can't be renamed, tagged, or linked", http.StatusBadRequest)
		return
	}
	body, ok := decodePatch(w, r)
	if !ok {
		return
	}
	var err error
	if body.Name != nil {
		err = s.Scanner.RenameCollection(r.Context(), id, *body.Name)
	}
	if err == nil && body.Tags != nil {
		err = s.DB.SetCollectionTags(r.Context(), id, *body.Tags)
	}
	if err == nil && body.SourceURL != nil {
		err = s.DB.SetCollectionSourceURL(r.Context(), id, *body.SourceURL)
	}
	if s.patchError(w, r, err) {
		return
	}
	chain, err := s.DB.CollectionPath(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, chain[len(chain)-1])
}

// patchError writes the response for a failed update and reports whether it did.
func (s *Server) patchError(w http.ResponseWriter, r *http.Request, err error) bool {
	var invalid *library.InvalidError
	var conflict *library.ConflictError
	switch {
	case err == nil:
		return false
	case errors.Is(err, db.ErrNotFound):
		http.NotFound(w, r)
	case errors.As(err, &invalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &conflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		serverError(w, err)
	}
	return true
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, toJSON(m))
}

func (s *Server) modelFile(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	// The path comes from our own scan of the library, never from the request.
	f, err := os.Open(filepath.Join(s.Scanner.Root, filepath.FromSlash(m.Path)))
	if err != nil {
		http.Error(w, "file unavailable; try rescanning", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType(m))
	w.Header().Set("ETag", `"`+m.Hash+`"`)
	w.Header().Set("Cache-Control", "no-cache") // revalidate via ETag; files can change in place
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filepath.Base(m.Path), `"`, "")+`"`)
	}
	// The viewer fetches files too; only explicit downloads count, and only
	// once per download (resumed ranges don't).
	if r.URL.Query().Has("download") && r.Method == http.MethodGet && startsAtZero(r.Header.Get("Range")) {
		s.recordDownload(r, db.Download{Kind: "model", ModelID: &m.ID, Path: m.Path})
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// modelMesh serves any model as a single merged binary STL.
func (s *Server) modelMesh(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	etag := `"stl-` + m.Hash + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	select {
	case meshSlots <- struct{}{}:
		defer func() { <-meshSlots }()
	case <-r.Context().Done():
		return
	}
	mm, err := mesh.Load(filepath.Join(s.Scanner.Root, filepath.FromSlash(m.Path)), m.Format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", contentTypes["stl"])
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if err := mesh.WriteBinarySTL(w, mm); err != nil {
		slog.Warn("write mesh", "path", m.Path, "err", err)
	}
}

type deleteBody struct {
	Models      []int64 `json:"models"`
	Collections []int64 `json:"collections"`
	// ExpectFiles is the file count from the preview the user confirmed.
	ExpectFiles *int `json:"expectFiles"`
}

// previewDelete reports what deleting the selection would remove.
func (s *Server) previewDelete(w http.ResponseWriter, r *http.Request) {
	var body deleteBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	plan, err := s.Scanner.PlanDelete(r.Context(), body.Models, body.Collections)
	if !s.patchError(w, r, err) {
		writeJSON(w, plan)
	}
}

// deleteItems deletes the selected files and folders from disk. It requires
// the file count from a preview, and refuses if that no longer matches.
func (s *Server) deleteItems(w http.ResponseWriter, r *http.Request) {
	var body deleteBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.ExpectFiles == nil {
		http.Error(w, "expectFiles is required; preview the deletion first", http.StatusBadRequest)
		return
	}
	plan, err := s.Scanner.Delete(r.Context(), body.Models, body.Collections, *body.ExpectFiles)
	if !s.patchError(w, r, err) {
		writeJSON(w, plan)
	}
}

func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	batches, err := s.Scanner.Trash()
	if err != nil {
		serverError(w, err)
		return
	}
	files, bytes := 0, int64(0)
	for _, b := range batches {
		files += b.Files
		bytes += b.Bytes
	}
	writeJSON(w, map[string]any{"batches": batches, "files": files, "bytes": bytes})
}

func (s *Server) restoreTrash(w http.ResponseWriter, r *http.Request) {
	b, err := s.Scanner.RestoreTrash(r.Context(), r.PathValue("id"))
	if errors.Is(err, library.ErrBatchNotFound) {
		http.NotFound(w, r)
		return
	}
	if !s.patchError(w, r, err) {
		writeJSON(w, b)
	}
}

// purgeTrash permanently deletes one trash batch, or with no id, empties the trash.
func (s *Server) purgeTrash(w http.ResponseWriter, r *http.Request) {
	err := s.Scanner.PurgeTrash(r.PathValue("id"))
	if errors.Is(err, library.ErrBatchNotFound) {
		http.NotFound(w, r)
		return
	}
	if !s.patchError(w, r, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// listDuplicates returns groups of identical files. limit=0 returns only the
// total, for the header badge.
func (s *Server) listDuplicates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	groups, total, err := s.DB.DuplicateGroups(r.Context(),
		clamp(atoiDefault(q.Get("limit"), 50), 0, 200), max(atoiDefault(q.Get("offset"), 0), 0))
	if err != nil {
		serverError(w, err)
		return
	}
	type groupJSON struct {
		Hash      string      `json:"hash"`
		Size      int64       `json:"size"`
		Thumbnail string      `json:"thumbnail,omitempty"`
		Models    []modelJSON `json:"models"`
	}
	out := make([]groupJSON, len(groups))
	for i, g := range groups {
		out[i] = groupJSON{Hash: g.Hash, Size: g.Size, Models: make([]modelJSON, len(g.Models))}
		for j, m := range g.Models {
			out[i].Models[j] = toJSON(m)
		}
		out[i].Thumbnail = out[i].Models[0].Thumbnail
	}
	writeJSON(w, map[string]any{"groups": out, "total": total})
}

// resolveDuplicates keeps one copy of a file and deletes the others from disk.
func (s *Server) resolveDuplicates(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !isHexHash(hash) {
		http.NotFound(w, r)
		return
	}
	var body struct {
		KeepID int64 `json:"keepId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.KeepID == 0 {
		http.Error(w, `body must be {"keepId": <model id>}`, http.StatusBadRequest)
		return
	}
	res, err := s.Scanner.ResolveDuplicates(r.Context(), hash, body.KeepID)
	if errors.Is(err, library.ErrNotInGroup) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		slog.Error("resolve duplicates", "hash", hash, "err", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, res)
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.DB.ListTags(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, tags)
}

func (s *Server) rescan(w http.ResponseWriter, r *http.Request) {
	res, err := s.Scanner.Scan(r.Context())
	if err != nil {
		// Scan errors (unmounted library, permissions) are meant for the user.
		slog.Error("rescan failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	hash, ok := strings.CutSuffix(r.PathValue("file"), ".png")
	if !ok || !isHexHash(hash) {
		http.NotFound(w, r)
		return
	}
	// URLs are content-addressed, so the browser may cache them forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, s.Thumbs.Path(hash))
}

func (s *Server) webHandler() http.Handler {
	if s.Web != nil {
		if _, err := fs.Stat(s.Web, "index.html"); err == nil {
			return http.FileServerFS(s.Web)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("stilton: the web UI isn't built into this binary.\n" +
			"Run `make build`, or use the Vite dev server (`make dev-web`).\n"))
	})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (db.Model, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return db.Model{}, false
	}
	m, err := s.DB.GetModel(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return m, false
	}
	if err != nil {
		serverError(w, err)
		return m, false
	}
	return m, true
}

func truthy(v string) bool { return v == "1" || v == "true" }

// contentType picks a Content-Type for an indexed file.
func contentType(m db.Model) string {
	if t, ok := contentTypes[m.Format]; ok {
		return t
	}
	if t := mime.TypeByExtension(path.Ext(m.Path)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// startsAtZero reports whether a Range header (possibly empty) asks for the
// start of the file, i.e. this is a new download rather than a resumption.
func startsAtZero(rangeHeader string) bool {
	return rangeHeader == "" || strings.HasPrefix(rangeHeader, "bytes=0-")
}

// recordDownload logs a download; failing to log never fails the download.
func (s *Server) recordDownload(r *http.Request, dl db.Download) {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		dl.RemoteAddr = host
	}
	if err := s.DB.RecordDownload(context.WithoutCancel(r.Context()), dl); err != nil {
		slog.Warn("record download", "path", dl.Path, "err", err)
	}
}

// storedExts are already compressed, so zipping them again only costs CPU.
var storedExts = map[string]bool{
	".3mf": true, ".zip": true, ".7z": true, ".rar": true, ".gz": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".mp4": true, ".mov": true, ".bgcode": true,
}

// downloadCollection streams a folder as a zip of everything in it (except
// hidden files), with the folder itself as the top-level entry.
func (s *Server) downloadCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := parseCollectionID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, err := s.DB.GetCollection(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if c.ParentID == nil {
		http.Error(w, "the whole library can't be downloaded as one zip; pick a folder", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.Scanner.Root, filepath.FromSlash(c.Path))
	if _, err := os.Stat(dir); err != nil {
		http.Error(w, "folder unavailable; try rescanning", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": c.Name + ".zip"}))
	if r.Method == http.MethodHead {
		return
	}
	zw := zip.NewWriter(w)
	base := filepath.Dir(dir)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		if storedExts[strings.ToLower(filepath.Ext(p))] {
			hdr.Method = zip.Store
		}
		dst, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(dst, f)
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		// Headers are already sent; abort the connection so the client sees
		// a failed download rather than a truncated zip.
		slog.Warn("folder download failed", "path", c.Path, "err", err)
		panic(http.ErrAbortHandler)
	}
	s.recordDownload(r, db.Download{Kind: "collection", CollectionID: &c.ID, Path: c.Path})
}

// listDownloads returns the most recent downloads.
func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	downloads, err := s.DB.RecentDownloads(r.Context(), clamp(atoiDefault(r.URL.Query().Get("limit"), 50), 1, 500))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, downloads)
}

// parseCollectionID accepts a numeric id or "root".
func parseCollectionID(s string) (int64, bool) {
	if s == "root" {
		return db.RootCollectionID, true
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// normalizeSourceURL validates a user-supplied source link. Only http(s) URLs
// are accepted since they're rendered as links; a bare "printables.com/..."
// gets https:// prepended. "" clears the link.
func normalizeSourceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 2048 {
		return "", errors.New("source URL is too long")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", errors.New("source URL must be an http or https link")
	}
	return u.String(), nil
}

func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		key := strings.ToLower(t)
		if t == "" || len(t) > 64 || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}

func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json", "err", err)
	}
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func clamp(n, lo, hi int) int {
	return min(max(n, lo), hi)
}
