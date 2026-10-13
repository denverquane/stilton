// Package db owns the SQLite database: connection setup, migrations, and queries.
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Thumbnail states stored in models.thumb_state.
const (
	ThumbPending = "pending"
	ThumbOK      = "ok"
	ThumbNone    = "none" // files with nothing to render, e.g. datapackage.json
	ThumbFailed  = "failed"
)

// Kinds stored in models.kind. Files are everything that isn't a 3D model:
// images, notes, project files. They're hidden from listings unless asked for.
const (
	KindModel = "model"
	KindFile  = "file"
)

// RootCollectionID is the collection for the library root; it always exists.
const RootCollectionID = 1

var ErrNotFound = errors.New("not found")

type DB struct {
	sql *sql.DB
}

type Model struct {
	ID           int64    `json:"id"`
	CollectionID int64    `json:"collectionId"`
	Path         string   `json:"path"` // relative to the library root, slash-separated
	Name         string   `json:"name"`
	Format       string   `json:"format"` // stl | obj | 3mf
	Size         int64    `json:"size"`
	Mtime        int64    `json:"mtime"`
	Hash         string   `json:"hash"`
	ThumbState   string   `json:"thumbState"`
	ThumbError   string   `json:"thumbError,omitempty"`
	AddedAt      int64    `json:"addedAt"`
	SourceURL    string   `json:"sourceUrl"` // where the user got it; optional
	Tags         []string `json:"tags"`
	Copies       int      `json:"copies"` // models with identical contents, including this one
	Kind         string   `json:"kind"`   // KindModel or KindFile
	Up           string   `json:"up"`     // see ValidUp; "" means unknown (treated as +z)
	ThumbKey     string   `json:"-"`      // names the thumbnail; see ThumbKey
	DownloadStats
}

// ValidUp reports whether up names an axis direction: "+z", "-y", and so on.
// "" (unknown) is also accepted.
func ValidUp(up string) bool {
	return up == "" || len(up) == 2 && (up[0] == '+' || up[0] == '-') && strings.ContainsRune("xyz", rune(up[1]))
}

// ThumbKey names a model's thumbnail: its hash, plus its orientation when
// that isn't the default. Must match the thumb_key column in the schema.
func ThumbKey(hash, up string) string {
	if up == "" || up == "+z" {
		return hash
	}
	sign := "p"
	if up[0] == '-' {
		sign = "n"
	}
	return hash + "_" + sign + up[1:]
}

// DownloadStats summarizes the downloads of a model or collection.
type DownloadStats struct {
	DownloadCount    int    `json:"downloadCount"`
	LastDownloadedAt *int64 `json:"lastDownloadedAt"` // unix seconds; nil if never
}

// Collection is a folder in the library that contains models somewhere beneath it.
type Collection struct {
	ID        int64    `json:"id"`
	ParentID  *int64   `json:"parentId"` // nil for the root
	Path      string   `json:"path"`     // relative to the library root; "" for the root
	Name      string   `json:"name"`
	SourceURL string   `json:"sourceUrl"`
	Tags      []string `json:"tags"`
	DownloadStats
}

// CollectionSummary describes a child collection for display as a folder card.
type CollectionSummary struct {
	Collection
	ModelCount   int      `json:"modelCount"`   // models anywhere beneath it
	FileCount    int      `json:"fileCount"`    // other files anywhere beneath it
	PendingCount int      `json:"pendingCount"` // of those, thumbnails not yet rendered
	PreviewKeys  []string `json:"-"`            // thumbnail keys
}

func Open(dataDir string) (*DB, error) {
	dsn := "file:" + filepath.Join(dataDir, "stilton.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{sql: sqlDB}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		if errors.Is(err, errStaleSchema) {
			return nil, fmt.Errorf("%s was created by an older version: %w. Delete it (and its -wal/-shm files) and restart; thumbnails are kept", filepath.Join(dataDir, "stilton.db"), err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// schemaFiles are the schema scripts, applied in name order (so number them
// 0001_, 0002_, ...). PRAGMA user_version records how many have run.
//
//go:embed schema/*.sql
var schemaFiles embed.FS

func (d *DB) migrate() error {
	scripts, err := schemaScripts()
	if err != nil {
		return err
	}
	var version int
	if err := d.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(scripts) {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d)", version, len(scripts))
	}
	for i := version; i < len(scripts); i++ {
		tx, err := d.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(scripts[i].sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema %s: %w", scripts[i].name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return d.checkFingerprint(scripts)
}

type schemaScript struct{ name, sql string }

func schemaScripts() ([]schemaScript, error) {
	entries, err := fs.ReadDir(schemaFiles, "schema")
	if err != nil {
		return nil, err
	}
	var out []schemaScript
	for _, e := range entries { // ReadDir sorts by name
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := fs.ReadFile(schemaFiles, "schema/"+e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, schemaScript{e.Name(), string(data)})
	}
	return out, nil
}

// schemaFingerprint identifies the exact schema scripts a database was built
// from. It's stored in PRAGMA application_id, so a database created before a
// script was edited in place is detected instead of failing on a missing
// column later.
func schemaFingerprint(scripts []schemaScript) int32 {
	h := sha256.New()
	for _, s := range scripts {
		h.Write([]byte(s.name))
		h.Write([]byte{0})
		h.Write([]byte(s.sql))
	}
	sum := h.Sum(nil)
	return int32(binary.BigEndian.Uint32(sum[:4]))
}

func (d *DB) checkFingerprint(scripts []schemaScript) error {
	want := schemaFingerprint(scripts)
	var got int32
	if err := d.sql.QueryRow("PRAGMA application_id").Scan(&got); err != nil {
		return err
	}
	if got == want {
		return nil
	}
	if got == 0 {
		// Created before fingerprints existed. Adopt it if it has the newest
		// tables and columns; otherwise it's from an older schema.
		if _, err := d.sql.Exec(`SELECT kind FROM models LIMIT 0; SELECT 1 FROM downloads LIMIT 0`); err == nil {
			_, err = d.sql.Exec(fmt.Sprintf("PRAGMA application_id = %d", want))
			return err
		}
	}
	return errStaleSchema
}

var errStaleSchema = errors.New("its schema is out of date (the schema files changed since it was created)")

const modelColumns = `id, collection_id, kind, path, name, format, size, mtime, hash, thumb_state, thumb_error, added_at, source_url, up, thumb_key`

func scanModel(row interface{ Scan(...any) error }) (Model, error) {
	var m Model
	err := row.Scan(&m.ID, &m.CollectionID, &m.Kind, &m.Path, &m.Name, &m.Format, &m.Size, &m.Mtime, &m.Hash,
		&m.ThumbState, &m.ThumbError, &m.AddedAt, &m.SourceURL, &m.Up, &m.ThumbKey)
	return m, err
}

const collectionColumns = `id, parent_id, path, name, source_url`

func scanCollection(row interface{ Scan(...any) error }) (Collection, error) {
	var c Collection
	err := row.Scan(&c.ID, &c.ParentID, &c.Path, &c.Name, &c.SourceURL)
	return c, err
}

// FileInfo is the subset of a model the scanner needs to decide whether a file changed.
type FileInfo struct {
	ID           int64
	CollectionID int64
	Size         int64
	Mtime        int64
	Hash         string
}

// KnownFiles returns size/mtime for every indexed path.
func (d *DB) KnownFiles(ctx context.Context) (map[string]FileInfo, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, collection_id, path, size, mtime, hash FROM models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]FileInfo{}
	for rows.Next() {
		var p string
		var fi FileInfo
		if err := rows.Scan(&fi.ID, &fi.CollectionID, &p, &fi.Size, &fi.Mtime, &fi.Hash); err != nil {
			return nil, err
		}
		known[p] = fi
	}
	return known, rows.Err()
}

// RelocateModel records that a model's file moved (same contents, new path),
// keeping its id and so its tags, source link, and download history.
func (d *DB) RelocateModel(ctx context.Context, id int64, m Model) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE models SET collection_id = ?, kind = ?, path = ?, name = ?, format = ?, size = ?, mtime = ?
		WHERE id = ?`, m.CollectionID, m.Kind, m.Path, m.Name, m.Format, m.Size, m.Mtime, id)
	return err
}

// CollectionIDs returns the id of every collection, by path.
func (d *DB) CollectionIDs(ctx context.Context) (map[string]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT path, id FROM collections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}

// TransferCollection gives toID the tags, source link (if it has none), and
// download history of fromID, for a folder that was moved or renamed.
func (d *DB) TransferCollection(ctx context.Context, fromID, toID int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`INSERT OR IGNORE INTO collection_tags (collection_id, tag_id) SELECT ?, tag_id FROM collection_tags WHERE collection_id = ?`,
		`UPDATE collections SET source_url = (SELECT source_url FROM collections WHERE id = ?2)
			WHERE id = ?1 AND source_url = ''`,
		`UPDATE downloads SET collection_id = ?1 WHERE collection_id = ?2`,
	} {
		if _, err := tx.ExecContext(ctx, q, toID, fromID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertModel inserts a new model or updates a changed one. A content change
// (different hash) resets the thumbnail to pending.
func (d *DB) UpsertModel(ctx context.Context, m Model) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO models (collection_id, kind, path, name, format, size, mtime, hash, thumb_state, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			collection_id = excluded.collection_id,
			kind = excluded.kind,
			size = excluded.size,
			mtime = excluded.mtime,
			thumb_state = CASE WHEN models.hash = excluded.hash THEN models.thumb_state ELSE excluded.thumb_state END,
			thumb_error = CASE WHEN models.hash = excluded.hash THEN models.thumb_error ELSE '' END,
			hash = excluded.hash`,
		m.CollectionID, m.Kind, m.Path, m.Name, m.Format, m.Size, m.Mtime, m.Hash, m.ThumbState, time.Now().Unix())
	return err
}

func (d *DB) DeleteModelsByPath(ctx context.Context, paths []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE path = ?`, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingThumbnails returns models that still need a thumbnail.
func (d *DB) PendingThumbnails(ctx context.Context, limit int) ([]Model, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+modelColumns+` FROM models WHERE thumb_state = ? ORDER BY id LIMIT ?`,
		ThumbPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetThumbState records the thumbnail result for every model with the given
// thumbnail key, since identical files in the same orientation share one.
func (d *DB) SetThumbState(ctx context.Context, key, state, errMsg string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE models SET thumb_state = ?, thumb_error = ? WHERE thumb_key = ?`, state, errMsg, key)
	return err
}

// ResetThumbnails marks every rendered or failed thumbnail as pending, for
// when the renderer changes.
func (d *DB) ResetThumbnails(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE models SET thumb_state = 'pending', thumb_error = '' WHERE thumb_state IN ('ok', 'failed')`)
	return err
}

// SetModelUp records which axis of a 3D model points up. Its thumbnail is
// re-rendered for the new orientation.
func (d *DB) SetModelUp(ctx context.Context, id int64, up string) error {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE models SET up = ?, thumb_state = 'pending', thumb_error = '' WHERE id = ? AND kind = 'model'`, up, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type ListQuery struct {
	Search       string
	CollectionID int64 // 0 means the whole library
	Recursive    bool  // include models in descendant collections
	// Tag matches models that carry it or sit in a collection that does
	// (directly or through an ancestor).
	Tag string
	// ExcludeTagged drops models inside collections carrying Tag, for views
	// that show those collections as folders instead.
	ExcludeTagged bool
	// IncludeFiles lists non-model files too, after the models.
	IncludeFiles bool
	Sort         string // SortName (default), SortDownloaded, or SortAdded
	Limit        int
	Offset       int
}

// Sort orders for listings.
const (
	SortName       = "name"
	SortDownloaded = "downloaded" // most recently downloaded first; never-downloaded last
	SortAdded      = "added"      // most recently indexed first
)

// subtreeCTE selects the ids of a collection and all its descendants into "subtree".
const subtreeCTE = `WITH RECURSIVE subtree(id) AS (
	SELECT ? UNION ALL SELECT c.id FROM collections c JOIN subtree ON c.parent_id = subtree.id)`

// coveredCTE selects into "covered" every collection that carries a tag (the
// bound parameter) or sits beneath one that does. Use it inside WITH RECURSIVE.
const coveredCTE = `covered(id) AS (
	SELECT ct.collection_id FROM collection_tags ct JOIN tags t ON t.id = ct.tag_id WHERE t.name = ?
	UNION
	SELECT c.id FROM collections c JOIN covered ON c.parent_id = covered.id)`

func (d *DB) ListModels(ctx context.Context, q ListQuery) ([]Model, int, error) {
	var where []string
	var args []any
	if s := strings.TrimSpace(q.Search); s != "" {
		where = append(where, `m.id IN (SELECT rowid FROM models_fts WHERE models_fts MATCH ?)`)
		args = append(args, ftsQuery(s))
	}
	if q.CollectionID != 0 && q.Recursive {
		where = append(where, `m.collection_id IN (`+subtreeCTE+` SELECT id FROM subtree)`)
		args = append(args, q.CollectionID)
	} else if q.CollectionID != 0 {
		where = append(where, `m.collection_id = ?`)
		args = append(args, q.CollectionID)
	}
	if !q.IncludeFiles {
		where = append(where, `m.kind = 'model'`)
	}
	if q.Tag != "" {
		where = append(where, `(m.id IN (SELECT mt.model_id FROM model_tags mt JOIN tags t ON t.id = mt.tag_id WHERE t.name = ?)
			OR m.collection_id IN (WITH RECURSIVE `+coveredCTE+` SELECT id FROM covered))`)
		args = append(args, q.Tag, q.Tag)
		if q.ExcludeTagged {
			where = append(where, `m.collection_id NOT IN (WITH RECURSIVE `+coveredCTE+` SELECT id FROM covered)`)
			args = append(args, q.Tag)
		}
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM models m`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+prefixed("m.", modelColumns)+` FROM models m
		LEFT JOIN (SELECT model_id, max(downloaded_at) AS last FROM downloads GROUP BY model_id) dl ON dl.model_id = m.id`+
			clause+` ORDER BY `+modelOrder(q.Sort)+` LIMIT ? OFFSET ?`,
		append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	models := []Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, 0, err
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := d.attachDetails(ctx, models); err != nil {
		return nil, 0, err
	}
	return models, total, nil
}

// modelOrder is the ORDER BY for a listing. Models come before files, and
// name breaks ties. Expects the downloads aggregate joined as "dl".
func modelOrder(sort string) string {
	const byName = `m.kind = 'file', m.name COLLATE NOCASE, m.id`
	switch sort {
	case SortDownloaded:
		return `dl.last IS NULL, dl.last DESC, ` + byName
	case SortAdded:
		return `m.added_at DESC, ` + byName
	}
	return byName
}

func (d *DB) GetModel(ctx context.Context, id int64) (Model, error) {
	m, err := scanModel(d.sql.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM models WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	ms := []Model{m}
	err = d.attachDetails(ctx, ms)
	return ms[0], err
}

// tagLink describes a table that attaches tags to models or collections.
// Fields are fixed identifiers, never user input.
type tagLink struct{ table, ownerCol, ownerTable string }

var (
	modelTags      = tagLink{"model_tags", "model_id", "models"}
	collectionTags = tagLink{"collection_tags", "collection_id", "collections"}
)

// tagsFor returns the tag names attached to each of ids, with every id present.
func (d *DB) tagsFor(ctx context.Context, l tagLink, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		out[id] = []string{}
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT x.`+l.ownerCol+`, t.name FROM `+l.table+` x JOIN tags t ON t.id = x.tag_id
		WHERE x.`+l.ownerCol+` IN (`+strings.Join(placeholders, ",")+`) ORDER BY t.name COLLATE NOCASE`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// attachDetails fills in each model's tags and duplicate count.
func (d *DB) attachDetails(ctx context.Context, models []Model) error {
	if err := d.attachTags(ctx, models); err != nil {
		return err
	}
	if len(models) == 0 {
		return nil
	}
	placeholders := make([]string, len(models))
	args := make([]any, len(models))
	for i, m := range models {
		placeholders[i] = "?"
		args[i] = m.Hash
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT hash, count(*) FROM models WHERE kind = 'model' AND hash IN (`+strings.Join(placeholders, ",")+`) GROUP BY hash`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	copies := map[string]int{}
	for rows.Next() {
		var hash string
		var n int
		if err := rows.Scan(&hash, &n); err != nil {
			return err
		}
		copies[hash] = n
	}
	for i := range models {
		models[i].Copies = copies[models[i].Hash]
	}
	if err := rows.Err(); err != nil {
		return err
	}

	ids := make([]int64, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	stats, err := d.downloadStats(ctx, "model_id", ids)
	if err != nil {
		return err
	}
	for i := range models {
		models[i].DownloadStats = stats[models[i].ID]
	}
	return nil
}

// downloadStats summarizes downloads per id, where col is "model_id" or
// "collection_id" (fixed identifiers, never user input).
func (d *DB) downloadStats(ctx context.Context, col string, ids []int64) (map[int64]DownloadStats, error) {
	out := map[int64]DownloadStats{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+col+`, count(*), max(downloaded_at) FROM downloads
		WHERE `+col+` IN (`+strings.Join(placeholders, ",")+`) GROUP BY `+col, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st DownloadStats
		var last int64
		if err := rows.Scan(&id, &st.DownloadCount, &last); err != nil {
			return nil, err
		}
		st.LastDownloadedAt = &last
		out[id] = st
	}
	return out, rows.Err()
}

// Download is one download of a model file or a collection.
type Download struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"` // "model" or "collection"
	ModelID      *int64 `json:"modelId"`
	CollectionID *int64 `json:"collectionId"`
	Path         string `json:"path"`
	UserID       *int64 `json:"userId"`
	RemoteAddr   string `json:"remoteAddr"`
	DownloadedAt int64  `json:"downloadedAt"`
}

// RecordDownload logs a download, stamping it with the current time.
func (d *DB) RecordDownload(ctx context.Context, dl Download) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO downloads (kind, model_id, collection_id, path, user_id, remote_addr, downloaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		dl.Kind, dl.ModelID, dl.CollectionID, dl.Path, dl.UserID, dl.RemoteAddr, time.Now().Unix())
	return err
}

// RecentDownloads returns the latest downloads, newest first.
func (d *DB) RecentDownloads(ctx context.Context, limit int) ([]Download, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, kind, model_id, collection_id, path, user_id, remote_addr, downloaded_at
		FROM downloads ORDER BY downloaded_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Download{}
	for rows.Next() {
		var dl Download
		if err := rows.Scan(&dl.ID, &dl.Kind, &dl.ModelID, &dl.CollectionID, &dl.Path, &dl.UserID, &dl.RemoteAddr, &dl.DownloadedAt); err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

func (d *DB) attachTags(ctx context.Context, models []Model) error {
	ids := make([]int64, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	tags, err := d.tagsFor(ctx, modelTags, ids)
	if err != nil {
		return err
	}
	for i := range models {
		models[i].Tags = tags[models[i].ID]
	}
	return nil
}

// attachCollectionDetails fills in each collection's tags and download stats.
func (d *DB) attachCollectionDetails(ctx context.Context, cs []*Collection) error {
	if err := d.attachCollectionTags(ctx, cs); err != nil {
		return err
	}
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	stats, err := d.downloadStats(ctx, "collection_id", ids)
	if err != nil {
		return err
	}
	for _, c := range cs {
		c.DownloadStats = stats[c.ID]
	}
	return nil
}

func (d *DB) attachCollectionTags(ctx context.Context, cs []*Collection) error {
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	tags, err := d.tagsFor(ctx, collectionTags, ids)
	if err != nil {
		return err
	}
	for _, c := range cs {
		c.Tags = tags[c.ID]
	}
	return nil
}

// SetModelTags replaces a model's tags.
func (d *DB) SetModelTags(ctx context.Context, id int64, tags []string) error {
	return d.setTags(ctx, modelTags, id, tags)
}

// SetCollectionTags replaces a collection's tags.
func (d *DB) SetCollectionTags(ctx context.Context, id int64, tags []string) error {
	return d.setTags(ctx, collectionTags, id, tags)
}

func (d *DB) setTags(ctx context.Context, l tagLink, id int64, tags []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+l.ownerTable+` WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+l.table+` WHERE `+l.ownerCol+` = ?`, id); err != nil {
		return err
	}
	for _, t := range tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, t); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO `+l.table+` (`+l.ownerCol+`, tag_id) SELECT ?, id FROM tags WHERE name = ?`,
			id, t); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, pruneTagsSQL); err != nil {
		return err
	}
	return tx.Commit()
}

// BulkTags adds and removes tags on many models and collections at once.
// Only direct tags are affected; a tag inherited from a folder above can't be
// removed here. Nothing changes if any id doesn't exist.
func (d *DB) BulkTags(ctx context.Context, modelIDs, collectionIDs []int64, add, remove []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, l := range []struct {
		link tagLink
		ids  []int64
	}{{modelTags, modelIDs}, {collectionTags, collectionIDs}} {
		if len(l.ids) == 0 {
			continue
		}
		placeholders := make([]string, len(l.ids))
		ids := make([]any, len(l.ids))
		for i, id := range l.ids {
			placeholders[i] = "?"
			ids[i] = id
		}
		in := `(` + strings.Join(placeholders, ",") + `)`
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT id) FROM `+l.link.ownerTable+` WHERE id IN `+in, ids...).Scan(&n); err != nil {
			return err
		}
		if n != len(slices.Compact(slices.Sorted(slices.Values(l.ids)))) {
			return ErrNotFound
		}
		for _, t := range add {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, t); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO `+l.link.table+` (`+l.link.ownerCol+`, tag_id)
				SELECT o.id, t.id FROM `+l.link.ownerTable+` o, tags t WHERE o.id IN `+in+` AND t.name = ?`,
				append(ids, t)...); err != nil {
				return err
			}
		}
		for _, t := range remove {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM `+l.link.table+` WHERE `+l.link.ownerCol+` IN `+in+`
				AND tag_id IN (SELECT id FROM tags WHERE name = ?)`, append(ids, t)...); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, pruneTagsSQL); err != nil {
		return err
	}
	return tx.Commit()
}

// FilesNamed returns indexed non-model files with exactly this file name.
func (d *DB) FilesNamed(ctx context.Context, name string) ([]Model, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+modelColumns+` FROM models WHERE kind = 'file' AND name = ? ORDER BY path`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const pruneTagsSQL = `DELETE FROM tags
	WHERE id NOT IN (SELECT tag_id FROM model_tags) AND id NOT IN (SELECT tag_id FROM collection_tags)`

// PruneTags deletes tags no longer attached to anything, e.g. after the
// scanner removes models or collections.
func (d *DB) PruneTags(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, pruneTagsSQL)
	return err
}

// GetCollection returns one collection with its tags.
func (d *DB) GetCollection(ctx context.Context, id int64) (Collection, error) {
	c, err := scanCollection(d.sql.QueryRowContext(ctx, `SELECT `+collectionColumns+` FROM collections WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	return c, d.attachCollectionDetails(ctx, []*Collection{&c})
}

// CollectionExists reports whether a collection row exists at path.
func (d *DB) CollectionExists(ctx context.Context, path string) (bool, error) {
	var exists bool
	err := d.sql.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM collections WHERE path = ?)`, path).Scan(&exists)
	return exists, err
}

// CreateAndMoveInto records that models and collections were moved into a
// new folder at path, beneath parentID. Returns the new collection's id.
func (d *DB) CreateAndMoveInto(ctx context.Context, parentID int64, path, name string, models []Model, collections []Collection) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO collections (parent_id, path, name) VALUES (?, ?, ?) RETURNING id`, parentID, path, name).Scan(&id); err != nil {
		return 0, err
	}
	if err := moveRows(ctx, tx, id, path, models, collections); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// MoveInto records that models and collections were moved into the existing
// collection destID at destPath.
func (d *DB) MoveInto(ctx context.Context, destID int64, destPath string, models []Model, collections []Collection) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := moveRows(ctx, tx, destID, destPath, models, collections); err != nil {
		return err
	}
	return tx.Commit()
}

// moveRows updates rows in place, so tags and source links travel with them.
func moveRows(ctx context.Context, tx *sql.Tx, destID int64, destPath string, models []Model, collections []Collection) error {
	for _, c := range collections {
		newPath := pathpkg.Join(destPath, pathpkg.Base(c.Path))
		if err := rewriteDescendants(ctx, tx, c.Path, newPath); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collections SET path = ?, parent_id = ? WHERE id = ?`,
			newPath, destID, c.ID); err != nil {
			return err
		}
	}
	for _, m := range models {
		if _, err := tx.ExecContext(ctx, `UPDATE models SET path = ?, collection_id = ? WHERE id = ?`,
			pathpkg.Join(destPath, pathpkg.Base(m.Path)), destID, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// rewriteDescendants updates the paths of every collection and model beneath
// a folder that moved from oldPath to newPath (not the folder's own row).
func rewriteDescendants(ctx context.Context, tx *sql.Tx, oldPath, newPath string) error {
	oldPrefix, newPrefix := oldPath+"/", newPath+"/"
	// substr rather than LIKE: folder names often contain "_" and "%".
	for _, table := range []string{"collections", "models"} {
		if _, err := tx.ExecContext(ctx, `
			UPDATE `+table+` SET path = ? || substr(path, ?) WHERE substr(path, 1, ?) = ?`,
			newPrefix, len(oldPrefix)+1, len(oldPrefix), oldPrefix); err != nil {
			return err
		}
	}
	return nil
}

// RenameModel records that a model's file now lives at newPath.
func (d *DB) RenameModel(ctx context.Context, id int64, newPath, newName string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE models SET path = ?, name = ? WHERE id = ?`, newPath, newName, id)
	return err
}

// RenameCollection records that a collection's folder now lives at newPath,
// along with everything inside it.
func (d *DB) RenameCollection(ctx context.Context, id int64, oldPath, newPath, newName string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := rewriteDescendants(ctx, tx, oldPath, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE collections SET path = ?, name = ? WHERE id = ?`, newPath, newName, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteItems removes index rows for deleted model files and folders.
// Models beneath each folder go first, since models don't cascade.
func (d *DB) DeleteItems(ctx context.Context, modelIDs []int64, collections []Collection) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range modelIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id); err != nil {
			return err
		}
	}
	for _, c := range collections {
		prefix := c.Path + "/"
		if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE substr(path, 1, ?) = ?`, len(prefix), prefix); err != nil {
			return err
		}
		// Child collections go via ON DELETE CASCADE.
		if _, err := tx.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, c.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AllThumbKeys returns the thumbnail key of every indexed model.
func (d *DB) AllThumbKeys(ctx context.Context) (map[string]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT thumb_key FROM models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// Subtree returns every collection and model at or beneath path, with tags.
func (d *DB) Subtree(ctx context.Context, path string) ([]Collection, []Model, error) {
	prefix := path + "/"
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+collectionColumns+` FROM collections WHERE path = ? OR substr(path, 1, ?) = ?`, path, len(prefix), prefix)
	if err != nil {
		return nil, nil, err
	}
	var cs []Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	ptrs := make([]*Collection, len(cs))
	for i := range cs {
		ptrs[i] = &cs[i]
	}
	if err := d.attachCollectionTags(ctx, ptrs); err != nil {
		return nil, nil, err
	}

	rows, err = d.sql.QueryContext(ctx, `
		SELECT `+modelColumns+` FROM models WHERE substr(path, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return nil, nil, err
	}
	var ms []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return cs, ms, d.attachTags(ctx, ms)
}

// ModelIDByPath returns the id of the model at path.
func (d *DB) ModelIDByPath(ctx context.Context, path string) (int64, error) {
	return d.idByPath(ctx, "models", path)
}

// CollectionIDByPath returns the id of the collection at path.
func (d *DB) CollectionIDByPath(ctx context.Context, path string) (int64, error) {
	return d.idByPath(ctx, "collections", path)
}

func (d *DB) idByPath(ctx context.Context, table, path string) (int64, error) {
	var id int64
	err := d.sql.QueryRowContext(ctx, `SELECT id FROM `+table+` WHERE path = ?`, path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// DuplicateGroup is a set of models with identical contents.
type DuplicateGroup struct {
	Hash   string  `json:"hash"`
	Size   int64   `json:"size"`
	Models []Model `json:"models"`
}

// DuplicateGroups lists sets of models sharing a hash, and how many sets exist.
func (d *DB) DuplicateGroups(ctx context.Context, limit, offset int) ([]DuplicateGroup, int, error) {
	const dupes = `SELECT hash FROM models WHERE kind = 'model' GROUP BY hash HAVING count(*) > 1`
	var total int
	if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM (`+dupes+`)`).Scan(&total); err != nil {
		return nil, 0, err
	}
	groups := []DuplicateGroup{}
	if limit <= 0 || total == 0 {
		return groups, total, nil
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+prefixed("m.", modelColumns)+` FROM models m
		JOIN (`+dupes+` ORDER BY min(name COLLATE NOCASE), hash LIMIT ? OFFSET ?) g ON g.hash = m.hash
		WHERE m.kind = 'model'
		ORDER BY m.hash, m.path COLLATE NOCASE`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	var models []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		models = append(models, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := d.attachDetails(ctx, models); err != nil {
		return nil, 0, err
	}
	for _, m := range models {
		if n := len(groups); n == 0 || groups[n-1].Hash != m.Hash {
			groups = append(groups, DuplicateGroup{Hash: m.Hash, Size: m.Size})
		}
		groups[len(groups)-1].Models = append(groups[len(groups)-1].Models, m)
	}
	// Groups were ordered by hash for grouping; restore name order.
	slices.SortStableFunc(groups, func(a, b DuplicateGroup) int {
		return strings.Compare(strings.ToLower(a.Models[0].Name), strings.ToLower(b.Models[0].Name))
	})
	return groups, total, nil
}

// ModelsByHash returns every model with the given contents.
func (d *DB) ModelsByHash(ctx context.Context, hash string) ([]Model, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+modelColumns+` FROM models WHERE hash = ? AND kind = 'model' ORDER BY path`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MergeMetadata copies the tags of the other models onto keepID, and adopts
// a source link from them if keepID has none.
func (d *DB) MergeMetadata(ctx context.Context, keepID int64, removeIDs []int64) error {
	if len(removeIDs) == 0 {
		return nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	placeholders := make([]string, len(removeIDs))
	ids := make([]any, len(removeIDs))
	for i, id := range removeIDs {
		placeholders[i] = "?"
		ids[i] = id
	}
	in := `(` + strings.Join(placeholders, ",") + `)`
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO model_tags (model_id, tag_id)
		SELECT ?, tag_id FROM model_tags WHERE model_id IN `+in, append([]any{keepID}, ids...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE models SET source_url = (
			SELECT source_url FROM models WHERE id IN `+in+` AND source_url != '' ORDER BY id LIMIT 1)
		WHERE id = ? AND source_url = ''
			AND EXISTS (SELECT 1 FROM models WHERE id IN `+in+` AND source_url != '')`,
		append(append(append([]any{}, ids...), keepID), ids...)...); err != nil {
		return err
	}
	return tx.Commit()
}

// SetModelSourceURL records where a model came from ("" clears it).
func (d *DB) SetModelSourceURL(ctx context.Context, id int64, url string) error {
	return d.setSourceURL(ctx, "models", id, url)
}

// SetCollectionSourceURL records where a collection came from ("" clears it).
func (d *DB) SetCollectionSourceURL(ctx context.Context, id int64, url string) error {
	return d.setSourceURL(ctx, "collections", id, url)
}

func (d *DB) setSourceURL(ctx context.Context, table string, id int64, url string) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE `+table+` SET source_url = ? WHERE id = ?`, url, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertCollection returns the id of the collection at path, creating it if needed.
func (d *DB) UpsertCollection(ctx context.Context, parentID int64, path, name string) (int64, error) {
	var id int64
	// DO UPDATE rather than DO NOTHING so RETURNING yields the existing row's id.
	err := d.sql.QueryRowContext(ctx, `
		INSERT INTO collections (parent_id, path, name) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET parent_id = excluded.parent_id, name = excluded.name
		RETURNING id`, parentID, path, name).Scan(&id)
	return id, err
}

// PruneCollections deletes collections with no models anywhere beneath them.
// Call it after removing deleted models. The root is never pruned.
func (d *DB) PruneCollections(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `
		DELETE FROM collections WHERE parent_id IS NOT NULL AND id NOT IN (
			WITH RECURSIVE keep(id) AS (
				SELECT collection_id FROM models
				UNION
				SELECT c.parent_id FROM collections c JOIN keep ON c.id = keep.id WHERE c.parent_id IS NOT NULL
			)
			SELECT id FROM keep
		)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CollectionPath returns the chain of collections from the root down to id.
func (d *DB) CollectionPath(ctx context.Context, id int64) ([]Collection, error) {
	rows, err := d.sql.QueryContext(ctx, `
		WITH RECURSIVE chain(id, depth) AS (
			SELECT ?, 0
			UNION ALL
			SELECT c.parent_id, chain.depth + 1 FROM collections c JOIN chain ON c.id = chain.id
			WHERE c.parent_id IS NOT NULL
		)
		SELECT `+prefixed("c.", collectionColumns)+` FROM chain JOIN collections c ON c.id = chain.id
		ORDER BY chain.depth DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chain []Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		chain = append(chain, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, ErrNotFound
	}
	ptrs := make([]*Collection, len(chain))
	for i := range chain {
		ptrs[i] = &chain[i]
	}
	return chain, d.attachCollectionDetails(ctx, ptrs)
}

// InTaggedCollection reports whether a collection or any of its ancestors
// carries tag, meaning everything inside it matches that tag.
func (d *DB) InTaggedCollection(ctx context.Context, id int64, tag string) (bool, error) {
	var tagged bool
	err := d.sql.QueryRowContext(ctx, `
		WITH RECURSIVE chain(id) AS (
			SELECT ?
			UNION ALL
			SELECT c.parent_id FROM collections c JOIN chain ON c.id = chain.id WHERE c.parent_id IS NOT NULL
		)
		SELECT EXISTS (
			SELECT 1 FROM chain
			JOIN collection_tags ct ON ct.collection_id = chain.id
			JOIN tags t ON t.id = ct.tag_id
			WHERE t.name = ?
		)`, id, tag).Scan(&tagged)
	return tagged, err
}

// ChildCollections lists a collection's children with model counts and up to
// previewCount distinct thumbnail hashes drawn from each child's whole subtree.
func (d *DB) ChildCollections(ctx context.Context, parentID int64, opts CollectionOpts) ([]CollectionSummary, error) {
	return d.summarizedCollections(ctx, opts, `
		SELECT `+collectionColumns+` FROM collections WHERE parent_id = ? ORDER BY name COLLATE NOCASE`, parentID)
}

// TaggedCollections lists the outermost collections carrying tag anywhere
// beneath withinID (excluding withinID itself), summarized like
// ChildCollections. Tagged collections nested in another tagged one are
// omitted; they're reached by opening the outer one.
func (d *DB) TaggedCollections(ctx context.Context, withinID int64, tag string, opts CollectionOpts) ([]CollectionSummary, error) {
	return d.summarizedCollections(ctx, opts, subtreeCTE+`, `+coveredCTE+`
		SELECT `+prefixed("c.", collectionColumns)+` FROM collections c
		WHERE c.id IN (SELECT id FROM subtree) AND c.id != ?
			AND c.id IN (SELECT ct.collection_id FROM collection_tags ct JOIN tags t ON t.id = ct.tag_id WHERE t.name = ?)
			AND c.parent_id NOT IN (SELECT id FROM covered)
		ORDER BY c.name COLLATE NOCASE`, withinID, tag, withinID, tag)
}

// summarizedCollections runs a query selecting collectionColumns and adds tags,
// model counts, and preview hashes to each result.
// CollectionOpts controls how child collections are summarized.
type CollectionOpts struct {
	Previews int // thumbnails per collection
	// IncludeFiles keeps collections that hold only non-model files, and lets
	// images fill in previews after model renders.
	IncludeFiles bool
	Sort         string // SortName (default) or SortDownloaded
}

func (d *DB) summarizedCollections(ctx context.Context, opts CollectionOpts, query string, args ...any) ([]CollectionSummary, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := []CollectionSummary{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, CollectionSummary{Collection: c})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	idx := make(map[int64]int, len(out))
	ptrs := make([]*Collection, len(out))
	placeholders := make([]string, len(out))
	ids := make([]any, len(out))
	for i := range out {
		idx[out[i].ID] = i
		ptrs[i] = &out[i].Collection
		placeholders[i] = "?"
		ids[i] = out[i].ID
	}
	if err := d.attachCollectionDetails(ctx, ptrs); err != nil {
		return nil, err
	}

	// tree maps every collection beneath each result to that result.
	treeCTE := `WITH RECURSIVE tree(root, id) AS (
		SELECT id, id FROM collections WHERE id IN (` + strings.Join(placeholders, ",") + `)
		UNION ALL
		SELECT tree.root, c.id FROM collections c JOIN tree ON c.parent_id = tree.id)`

	rows, err = d.sql.QueryContext(ctx, treeCTE+`
		SELECT tree.root, sum(m.kind = 'model'), sum(m.kind = 'file'), sum(m.thumb_state = 'pending')
		FROM tree JOIN models m ON m.collection_id = tree.id GROUP BY tree.root`, ids...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var models, files, pending int
		if err := rows.Scan(&id, &models, &files, &pending); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].ModelCount = models
		out[idx[id]].FileCount = files
		out[idx[id]].PendingCount = pending
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Identical files share a thumbnail, so pick distinct keys. Model
	// renders come first; images (only when files are shown) fill in after.
	kinds := `'model'`
	if opts.IncludeFiles {
		kinds = `'model', 'file'`
	}
	rows, err = d.sql.QueryContext(ctx, treeCTE+`
		SELECT root, thumb_key FROM (
			SELECT root, thumb_key, row_number() OVER (PARTITION BY root ORDER BY is_file, first_path) AS n
			FROM (
				SELECT tree.root, m.thumb_key, min(m.kind = 'file') AS is_file, min(m.path COLLATE NOCASE) AS first_path
				FROM tree JOIN models m ON m.collection_id = tree.id
				WHERE m.thumb_state = 'ok' AND m.kind IN (`+kinds+`)
				GROUP BY tree.root, m.thumb_key
			)
		) WHERE n <= ? ORDER BY root, n`, append(ids, opts.Previews)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var hash string
		if err := rows.Scan(&id, &hash); err != nil {
			rows.Close()
			return nil, err
		}
		out[idx[id]].PreviewKeys = append(out[idx[id]].PreviewKeys, hash)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if !opts.IncludeFiles {
		// Folders holding only other files are hidden along with the files.
		out = slices.DeleteFunc(out, func(c CollectionSummary) bool { return c.ModelCount == 0 })
	}
	if opts.Sort == SortDownloaded {
		slices.SortStableFunc(out, func(a, b CollectionSummary) int {
			switch {
			case a.LastDownloadedAt == nil && b.LastDownloadedAt == nil:
				return 0
			case a.LastDownloadedAt == nil:
				return 1
			case b.LastDownloadedAt == nil:
				return -1
			}
			return int(*b.LastDownloadedAt - *a.LastDownloadedAt)
		})
	}
	return out, nil
}

type TagCount struct {
	Name        string `json:"name"`
	Models      int    `json:"models"`
	Collections int    `json:"collections"`
}

func (d *DB) ListTags(ctx context.Context) ([]TagCount, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT t.name,
			(SELECT count(*) FROM model_tags WHERE tag_id = t.id),
			(SELECT count(*) FROM collection_tags WHERE tag_id = t.id)
		FROM tags t ORDER BY t.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Name, &tc.Models, &tc.Collections); err != nil {
			return nil, err
		}
		tags = append(tags, tc)
	}
	return tags, rows.Err()
}

// ftsQuery turns free text into an FTS5 prefix query, quoting each term so
// user input can't inject FTS syntax: `dragon head` -> `"dragon"* "head"*`.
func ftsQuery(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"*`
	}
	return strings.Join(fields, " ")
}

func prefixed(prefix, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}
