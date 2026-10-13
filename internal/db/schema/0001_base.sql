-- Base schema. Files in this directory are applied in name order, once each;
-- PRAGMA user_version records how many have run. Changing a file that has
-- already been applied makes existing databases refuse to start (see
-- schemaFingerprint), so until migrations are needed, edits here mean
-- deleting data/stilton.db.

-- Collections mirror the library's folders. Models reference their folder
-- without ON DELETE CASCADE, so pruning a non-empty collection fails loudly
-- instead of deleting models (and their tags).
CREATE TABLE collections (
	id         INTEGER PRIMARY KEY,
	parent_id  INTEGER REFERENCES collections(id) ON DELETE CASCADE,
	path       TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL,
	source_url TEXT NOT NULL DEFAULT ''
);
CREATE INDEX collections_parent ON collections(parent_id);
INSERT INTO collections (id, parent_id, path, name) VALUES (1, NULL, '', 'Library');

CREATE TABLE models (
	id            INTEGER PRIMARY KEY,
	collection_id INTEGER NOT NULL REFERENCES collections(id),
	kind          TEXT NOT NULL DEFAULT 'model',
	path          TEXT NOT NULL UNIQUE,
	name          TEXT NOT NULL,
	format        TEXT NOT NULL,
	size          INTEGER NOT NULL,
	mtime         INTEGER NOT NULL,
	hash          TEXT NOT NULL,
	thumb_state   TEXT NOT NULL DEFAULT 'pending',
	thumb_error   TEXT NOT NULL DEFAULT '',
	added_at      INTEGER NOT NULL,
	source_url    TEXT NOT NULL DEFAULT '',
	-- Which axis of the file points up: '+z' (or '' when unknown, treated the
	-- same), '-z', '+y', '-y', '+x', '-x'. Set by the user or a Manyfold import.
	up            TEXT NOT NULL DEFAULT '',
	-- Thumbnails depend on orientation as well as contents. Must match
	-- db.ThumbKey.
	thumb_key     TEXT GENERATED ALWAYS AS (
		CASE WHEN up IN ('', '+z') THEN hash
		ELSE hash || '_' || CASE substr(up, 1, 1) WHEN '+' THEN 'p' ELSE 'n' END || substr(up, 2, 1) END
	) VIRTUAL
);
CREATE INDEX models_hash ON models(hash);
CREATE INDEX models_thumb_key ON models(thumb_key);
CREATE INDEX models_collection ON models(collection_id);
CREATE INDEX models_thumb_state ON models(thumb_state);
CREATE INDEX models_kind ON models(kind);

CREATE TABLE tags (
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE
);
CREATE TABLE model_tags (
	model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
	tag_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	PRIMARY KEY (model_id, tag_id)
);
CREATE TABLE collection_tags (
	collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	tag_id        INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	PRIMARY KEY (collection_id, tag_id)
);

-- One row per download of a model file or a folder (as a zip). Ids are
-- nulled rather than cascaded when the item leaves the index, so history
-- survives; path records what it was called at the time. user_id is for
-- when there are accounts: nothing sets it yet, and there's no users table
-- to reference.
CREATE TABLE downloads (
	id            INTEGER PRIMARY KEY,
	kind          TEXT NOT NULL,  -- 'model' (any indexed file) or 'collection'
	model_id      INTEGER REFERENCES models(id) ON DELETE SET NULL,
	collection_id INTEGER REFERENCES collections(id) ON DELETE SET NULL,
	path          TEXT NOT NULL,
	user_id       INTEGER,
	remote_addr   TEXT NOT NULL DEFAULT '',
	downloaded_at INTEGER NOT NULL
);
CREATE INDEX downloads_model ON downloads(model_id, downloaded_at);
CREATE INDEX downloads_collection ON downloads(collection_id, downloaded_at);
CREATE INDEX downloads_time ON downloads(downloaded_at);

CREATE VIRTUAL TABLE models_fts USING fts5(name, path, content='models', content_rowid='id');
CREATE TRIGGER models_ai AFTER INSERT ON models BEGIN
	INSERT INTO models_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
END;
CREATE TRIGGER models_ad AFTER DELETE ON models BEGIN
	INSERT INTO models_fts(models_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
END;
CREATE TRIGGER models_au AFTER UPDATE OF name, path ON models BEGIN
	INSERT INTO models_fts(models_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
	INSERT INTO models_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
END;
