package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// writeLegacyStore creates a store in the original, Radarr-only schema: a
// single arr_indexer_id column holding Radarr's own local indexer id.
func writeLegacyStore(t *testing.T, hash string, radarrLocalID int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seedarr.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer legacy.Close()

	if _, err := legacy.Exec(`
		CREATE TABLE grabs (
			hash TEXT PRIMARY KEY,
			arr_indexer_id INTEGER NOT NULL,
			first_seen_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO grabs (hash, arr_indexer_id) VALUES (?, ?)`, hash, radarrLocalID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	return path
}

// writeInterimStore creates a store in the interim schema, from the design
// that keyed everything on Prowlarr's id: a NOT NULL prowlarr_indexer_id
// with no default — exactly the shape a store created fresh by that code
// has. Its Sonarr rows carry arr_indexer_id 0, since that design never
// resolved Sonarr's local id.
func writeInterimStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seedarr.db")

	interim, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer interim.Close()

	if _, err := interim.Exec(`
		CREATE TABLE grabs (
			hash TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			arr_indexer_id INTEGER NOT NULL,
			prowlarr_indexer_id INTEGER NOT NULL,
			first_seen_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		t.Fatalf("create interim table: %v", err)
	}
	if _, err := interim.Exec(`
		INSERT INTO grabs (hash, source, arr_indexer_id, prowlarr_indexer_id)
		VALUES ('movie', 'radarr', 10, 17), ('episode', 'sonarr', 0, 17)
	`); err != nil {
		t.Fatalf("insert interim rows: %v", err)
	}
	return path
}

func TestOpenGrabStore_LegacySchema_RowIsImmediatelyUsable(t *testing.T) {
	// The legacy column held Radarr's own local id (10 here), which is
	// exactly what arr_indexer_id means now — so the value stays put and
	// only the source it implies ("radarr", the only arr that existed) is
	// filled in. Nothing else is needed: the row is a complete key as is.
	store, err := openGrabStore(writeLegacyStore(t, "h1", 10))
	if err != nil {
		t.Fatalf("openGrabStore on a legacy db: %v", err)
	}
	defer store.close()

	g, known, err := store.grabFor("h1")
	if err != nil {
		t.Fatalf("grabFor after migration: %v", err)
	}
	want := grab{source: "radarr", arrIndexerID: 10}
	if !known || g != want {
		t.Fatalf("migrated row = %+v (known=%v), want %+v", g, known, want)
	}
}

func TestOpenGrabStore_InterimSchema_DropsTheProwlarrColumnAndKeepsRows(t *testing.T) {
	// The interim NOT NULL prowlarr_indexer_id column has to go: nothing
	// reads it any more, and an insert that omits it would fail against
	// it. The rows themselves survive the drop.
	store, err := openGrabStore(writeInterimStore(t))
	if err != nil {
		t.Fatalf("openGrabStore on an interim db: %v", err)
	}
	defer store.close()

	columns, err := store.columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if _, still := columns["prowlarr_indexer_id"]; still {
		t.Fatalf("expected prowlarr_indexer_id to be dropped, columns = %v", columns)
	}

	g, known, err := store.grabFor("movie")
	if err != nil {
		t.Fatalf("grabFor after migration: %v", err)
	}
	want := grab{source: "radarr", arrIndexerID: 10}
	if !known || g != want {
		t.Fatalf("interim radarr row = %+v (known=%v), want %+v", g, known, want)
	}

	// And the store takes new writes — the thing the lingering NOT NULL
	// column would have broken.
	if err := store.remember("h2", grab{source: "sonarr", arrIndexerID: 12}); err != nil {
		t.Fatalf("remember after migration: %v", err)
	}
}

func TestGrabFor_InterimSonarrRow_UnknownUntilRecaptured(t *testing.T) {
	// An interim Sonarr row has arr_indexer_id 0, because that design never
	// resolved Sonarr's local id. No arr has an indexer 0, so the row must
	// read as unknown rather than be looked up — the grab-events job
	// re-captures it with a real id on its next run, after which it
	// resolves normally.
	store, err := openGrabStore(writeInterimStore(t))
	if err != nil {
		t.Fatalf("openGrabStore on an interim db: %v", err)
	}
	defer store.close()

	if _, known, err := store.grabFor("episode"); err != nil || known {
		t.Fatalf("expected an unresolved interim sonarr row to read as unknown, got known=%v err=%v", known, err)
	}

	if err := store.remember("episode", grab{source: "sonarr", arrIndexerID: 12}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	g, known, _ := store.grabFor("episode")
	want := grab{source: "sonarr", arrIndexerID: 12}
	if !known || g != want {
		t.Fatalf("re-captured row = %+v (known=%v), want %+v", g, known, want)
	}
}

func TestOpenGrabStore_AlreadyCurrent_ReopenIsANoOp(t *testing.T) {
	// Opening a current store must not attempt any migration step again.
	path := filepath.Join(t.TempDir(), "seedarr.db")

	first, err := openGrabStore(path)
	if err != nil {
		t.Fatalf("openGrabStore: %v", err)
	}
	want := grab{source: "sonarr", arrIndexerID: 12}
	if err := first.remember("h1", want); err != nil {
		t.Fatalf("remember: %v", err)
	}
	first.close()

	second, err := openGrabStore(path)
	if err != nil {
		t.Fatalf("reopening an already-current store: %v", err)
	}
	defer second.close()

	if g, known, _ := second.grabFor("h1"); !known || g != want {
		t.Fatalf("expected the existing row to survive a reopen as %+v, got %+v known=%v", want, g, known)
	}
}

func TestGrabStore_ReGrabbedViaTheOtherArr_OverwritesProvenance(t *testing.T) {
	// A torrent first grabbed by Radarr and later re-grabbed by Sonarr must
	// end up recorded as Sonarr's — source and local id describe one grab,
	// so they have to move together, or the local id would be read against
	// the wrong arr.
	store := newTestStore(t)

	if err := store.remember("h1", grab{source: "radarr", arrIndexerID: 10}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	want := grab{source: "sonarr", arrIndexerID: 12}
	if err := store.remember("h1", want); err != nil {
		t.Fatalf("remember: %v", err)
	}

	if g, known, _ := store.grabFor("h1"); !known || g != want {
		t.Fatalf("re-grabbed row = %+v (known=%v), want %+v", g, known, want)
	}
}

func TestRememberAll_WritesEveryGrabInOneTransaction(t *testing.T) {
	store := newTestStore(t)

	grabs := map[string]grab{
		"h1": {source: "radarr", arrIndexerID: 10},
		"h2": {source: "sonarr", arrIndexerID: 12},
		"h3": {source: "radarr", arrIndexerID: 22},
	}
	n, err := store.rememberAll(grabs)
	if err != nil {
		t.Fatalf("rememberAll: %v", err)
	}
	if n != 3 {
		t.Fatalf("rememberAll wrote %d rows, want 3", n)
	}

	for hash, want := range grabs {
		if got, known, _ := store.grabFor(hash); !known || got != want {
			t.Fatalf("%s = %+v (known=%v), want %+v", hash, got, known, want)
		}
	}
}

func TestRememberAll_ReRunUpdatesExistingRows(t *testing.T) {
	// The job re-remembers everything both arrs can still see on every
	// run, so the same hashes are rewritten constantly — that has to
	// update in place, not collide on the primary key.
	store := newTestStore(t)

	if _, err := store.rememberAll(map[string]grab{"h1": {source: "radarr", arrIndexerID: 10}}); err != nil {
		t.Fatalf("rememberAll: %v", err)
	}
	want := grab{source: "sonarr", arrIndexerID: 12}
	if _, err := store.rememberAll(map[string]grab{"h1": want}); err != nil {
		t.Fatalf("rememberAll (second run): %v", err)
	}

	if got, known, _ := store.grabFor("h1"); !known || got != want {
		t.Fatalf("re-remembered row = %+v (known=%v), want %+v", got, known, want)
	}
}

func TestRememberAll_NothingToWrite_IsANoOp(t *testing.T) {
	// Both arrs down, or genuinely no history — must not open a
	// transaction just to commit nothing.
	store := newTestStore(t)

	n, err := store.rememberAll(nil)
	if err != nil || n != 0 {
		t.Fatalf("rememberAll(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestRememberAll_FailureLeavesTheStoreUntouched(t *testing.T) {
	// The whole point of one transaction: a failure part-way through must
	// roll the batch back, so the next run re-captures a clean set rather
	// than reasoning about a half-applied one. Forced by closing the
	// database underneath the write.
	store := newTestStore(t)
	if _, err := store.rememberAll(map[string]grab{"h1": {source: "radarr", arrIndexerID: 10}}); err != nil {
		t.Fatalf("rememberAll: %v", err)
	}

	store.db.Close()

	if _, err := store.rememberAll(map[string]grab{"h2": {source: "sonarr", arrIndexerID: 12}}); err == nil {
		t.Fatalf("expected rememberAll to fail against a closed database")
	}
}
