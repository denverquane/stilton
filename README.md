# stilton

Organization for your endless supply of 3D printing files.

Point stilton at a folder of STL, OBJ, and 3MF files and it indexes them into SQLite, renders a thumbnail for each one, and serves a web UI for browsing, searching, tagging, and viewing them in 3D. Your folder structure becomes nested collections.

## Running

Requires Go 1.26+ and Node 20.19+ (or 22.12+).

```sh
make build
./stilton -library ~/3d-models
```

Then open http://127.0.0.1:8080.

| Flag       | Default          | Description                                                                  |
| ---------- | ---------------- | ---------------------------------------------------------------------------- |
| `-library` | (required)       | Folder to index, recursively. Hidden files and folders are skipped.          |
| `-data`    | `data`           | Where the database (`stilton.db`) and thumbnails (`thumbs/`) live.           |
| `-addr`    | `127.0.0.1:8080` | Listen address. There is no auth, so be careful exposing it beyond localhost. |
| `-workers` | `2`              | Concurrent thumbnail renders. Large meshes can take hundreds of MB each.     |
| `-rescan`  | `5m`             | How often to rescan the library. `0` disables; the UI also has a Rescan button. |

stilton changes the library folder only when you ask it to: moving a selection (into an existing folder or a new one) moves those files and folders on disk, renaming renames them, and deleting a selection (or resolving duplicates) moves them into a hidden `.stilton-trash` folder at the top of the library. Emptying the trash is the only thing that permanently deletes files. Either way, folders left completely empty are removed.

## Development

Run the backend and the Vite dev server side by side:

```sh
make dev LIBRARY=~/3d-models   # Go server on :8080
make dev-web                   # UI with hot reload on :5173, proxying to :8080
```

`make test` runs the Go tests and type-checks the frontend.

## How it works

- **Scanning** (`internal/library`): walks the library, and for new or changed files (by size and mtime) computes a SHA-256 and upserts a row in `models`. Rows for deleted files are removed.
- **Other files**: every non-hidden file is indexed, not just STL/OBJ/3MF. Non-model files (images, notes, `datapackage.json`, project files) are hidden unless **Show other files** is ticked; then they appear as dashed cards you can select, move, rename, tag and delete like models, and folders containing only such files appear too. Images get thumbnails and fill in folder previews after model renders. Other files are never treated as duplicates.
- **Downloads**: every explicit download is recorded in the `downloads` table: a model file via its Download button, or a folder as a zip ("Download folder" in the folder bar). Loading a model into the 3D viewer, and resumed downloads, don't count. Each row keeps the path at the time, so history survives the item being deleted; `user_id` is reserved for when there are accounts. Listings can be sorted by name, most recently downloaded, or most recently added.
- **Collections**: every folder that contains models (at any depth) is a collection, nested like the folders are. Collections are created and pruned by the scanner, so a folder whose models are all deleted disappears. Browsing a collection shows its sub-collections, each previewed with up to four thumbnails from anywhere inside it, followed by the models directly in it. Searching lists matching models from the whole subtree. A model is in exactly one collection (its folder) but can have any number of tags.
- **Tags and source links**: both models and collections can have tags and an optional source link (http/https only; `printables.com/...` is read as `https://printables.com/...`). Filtering by a tag shows tagged collections as folders, the outermost ones only, and lists tagged models flat only when they aren't inside a tagged collection. Opening a tagged collection shows everything in it. Combined with a text search, a model matches a tag if it or any folder above it has the tag. Tags and links belong to the folder's path, so renaming or moving a folder (or emptying it until a rescan removes it) loses them, and the same is true for a model that's moved.
- **Organizing** (`internal/library/move.go`): select models and folders (tick the checkbox on a card; once anything is selected, clicking a card toggles it), then either **Move to…** any existing folder via a folder browser, or move them into a new folder inside the one you're viewing. You can also drag cards onto a folder card or a breadcrumb: dragging a selected card moves the whole selection, and dragging any other card moves just that one. With exactly one item selected, **Rename** renames the file (keeping its extension) or folder on disk. In search or tag results this gathers matches from anywhere. Index rows move with the files, so tags and source links are kept. Items already in the destination are skipped. The request is refused before anything moves if a name would clash with anything already in the destination, including unindexed files and folders, or if two selected items would clash with each other. Names are compared case-insensitively, and same-named folders are never merged. New folder names can't be hidden (start with `.`). If a move fails partway, completed moves are undone. Only model files and whole folders move; sidecar files next to a moved model (`datapackage.json`, images) stay where they are.
- **Deleting** (`internal/library/delete.go`): **Delete** in the selection bar works on any mix of files and folders. It first shows exactly what will go: each selected item with its file count, and a total split into model files and other files. Folders are deleted with everything in them, including files STLton doesn't show. Confirming sends the previewed file count back, and the server refuses (asking you to review again) if a folder's contents changed in the meantime.
- **Trash** (`internal/library/trash.go`): deleted items go to `.stilton-trash/<batch>/` inside the library, so even huge folders on a network share are trashed with a rename rather than a copy. Each deletion is one batch with a `manifest.json` recording the original paths plus the tags, source links and hashes of everything in it. The Trash page restores a batch to its original location, with tags and links reapplied (refused if something now occupies that spot), deletes a batch permanently, or empties the trash. Nothing is purged automatically.
- **Thumbnail sweep**: thumbnails no longer used by any indexed or trashed model are deleted after each scan that removes or changes files, after deletions, and when the trash is purged. Thumbnails for trashed models are kept so a restore doesn't re-render them.
- **Duplicates** (`internal/library/duplicates.go`): models with the same SHA-256 are duplicates. The UI shows how many sets need resolving until there are none. Resolving a set keeps the copy you pick and moves the others to the trash. Each one is re-hashed first, and any copy changed since the last scan is skipped. The kept copy inherits the deleted copies' tags, and their source link if it has none. Only byte-identical files count, so re-exports of the same mesh aren't detected.
- **Thumbnails** (`internal/thumbnail`): a background worker pool renders pending models with [fauxgl](https://github.com/fogleman/fauxgl), a pure-Go software rasterizer, so no GPU is needed. 3MFs exported by slicers usually embed a preview, which is used instead when present. Thumbnails are stored as `data/thumbs/<sha256>.png`, so identical files share one, and they're served with `Cache-Control: immutable`: the browser caches them forever and a changed file simply gets a new URL.
- **Viewer** (`web/src/viewer.ts`): three.js loads the original file directly. Models are treated as Z-up, like slicers do. For 3MFs three.js can't assemble, the server provides the merged geometry at `/api/models/{id}/mesh.stl`.
- **Database** (`internal/db`): SQLite via the pure-Go `modernc.org/sqlite` driver, so the binary has no cgo dependency. Schema migrations are tracked with `PRAGMA user_version`; search uses FTS5 over names and paths.
- The built frontend is embedded into the binary with `go:embed`.

## API

| Method | Path                         | Description                                           |
| ------ | ---------------------------- | ----------------------------------------------------- |
| GET    | `/api/models?collection=&q=&tag=&files=&sort=&limit=&offset=` | Models in a collection (id or `root`): directly in it, or from the whole subtree (`flat: true`) when searching or tag-filtering |
| POST   | `/api/collections`           | `{"parent": 12 or "root", "name": "Kit", "models": [...], "collections": [...]}`: make the folder and **move** those items into it |
| POST   | `/api/collections/{id}/move` | `{"models": [...], "collections": [...]}`: **move** those items into an existing collection (id or `root`) |
| GET    | `/api/collections/{id}?tag=&files=&sort=` | A collection (id or `root`), its breadcrumbs, and the folders to show in it with counts and previews |
| GET    | `/api/collections/{id}/download` | The folder as a zip (hidden files excluded); recorded as a download |
| GET    | `/api/downloads?limit=`      | Most recent downloads, newest first |
| PATCH  | `/api/collections/{id}`      | Update `{"name": "...", "tags": [...], "sourceUrl": "..."}`; omitted fields are unchanged. `name` **renames the folder on disk**. Not allowed on the root |
| GET    | `/api/models/{id}`           | One model                                             |
| GET    | `/api/models/{id}/file`      | Original file; `?download` sends it as an attachment and records the download |
| GET    | `/api/models/{id}/mesh.stl`  | Geometry merged into one binary STL                   |
| PATCH  | `/api/models/{id}`           | Update `{"name": "...", "tags": [...], "sourceUrl": "..."}`; omitted fields are unchanged. `name` **renames the file on disk**, keeping its extension |
| POST   | `/api/delete/preview`        | `{"models": [...], "collections": [...]}`: what deleting them would remove (file counts, bytes); deletes nothing |
| POST   | `/api/delete`                | Same body plus `"expectFiles"` from the preview: moves them to the trash; 409 if the count changed |
| GET    | `/api/trash`                 | Trash batches, newest first, with totals |
| POST   | `/api/trash/{id}/restore`    | Move a batch back to its original locations; 409 if one is occupied |
| DELETE | `/api/trash/{id}`, `/api/trash` | **Permanently delete** one batch, or empty the trash |
| GET    | `/api/duplicates?limit=&offset=` | Sets of identical files (`limit=0` for just the count) |
| POST   | `/api/duplicates/{sha256}/resolve` | `{"keepId": 12}`: keep that copy and move the others to the trash |
| GET    | `/api/tags`                  | All tags with counts                                  |
| POST   | `/api/rescan`                | Rescan the library now                                |
| GET    | `/thumbs/{sha256}.png`       | Thumbnail                                             |

## Ideas / next steps

- Watch the library with fsnotify instead of polling.
- Collections, notes, and print history per model.
- Multi-plate 3MFs: thumbnail and view individual plates.
- Upload files through the UI.
