package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchemaFingerprint(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var id int32
	d.sql.QueryRow("PRAGMA application_id").Scan(&id)
	scripts, _ := schemaScripts()
	if id != schemaFingerprint(scripts) || id == 0 {
		t.Fatalf("application_id = %d, want the schema fingerprint", id)
	}
	d.Close()

	// A database from before fingerprints (id 0) with the current tables is adopted.
	raw, _ := sql.Open("sqlite", "file:"+filepath.Join(dir, "stilton.db"))
	raw.Exec("PRAGMA application_id = 0")
	raw.Close()
	if d, err = Open(dir); err != nil {
		t.Fatalf("pre-fingerprint database: %v", err)
	}
	d.Close()

	// Built from different schema files: refused with instructions.
	raw, _ = sql.Open("sqlite", "file:"+filepath.Join(dir, "stilton.db"))
	raw.Exec("PRAGMA application_id = 12345")
	raw.Close()
	_, err = Open(dir)
	if !errors.Is(err, errStaleSchema) || !strings.Contains(err.Error(), "Delete it") {
		t.Fatalf("stale database: err = %v", err)
	}
}

// The thumb_key column and ThumbKey must name thumbnails identically.
func TestThumbKeyMatchesSchema(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	hash := strings.Repeat("ab", 32)
	for i, up := range []string{"", "+z", "-z", "+y", "-y", "+x", "-x"} {
		path := fmt.Sprintf("m%d.stl", i)
		if _, err := d.sql.Exec(`INSERT INTO models (collection_id, path, name, format, size, mtime, hash, added_at, up)
			VALUES (1, ?, 'm', 'stl', 1, 1, ?, 1, ?)`, path, hash, up); err != nil {
			t.Fatal(err)
		}
		var key string
		d.sql.QueryRow(`SELECT thumb_key FROM models WHERE path = ?`, path).Scan(&key)
		if want := ThumbKey(hash, up); key != want {
			t.Errorf("up %q: column %q, ThumbKey %q", up, key, want)
		}
		if !ValidUp(up) {
			t.Errorf("ValidUp(%q) = false", up)
		}
	}
	for _, bad := range []string{"z", "+w", "++", "+zz", "up"} {
		if ValidUp(bad) {
			t.Errorf("ValidUp(%q) = true", bad)
		}
	}
}
