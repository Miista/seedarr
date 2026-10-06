package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO — matches seedarr's static-binary build
)

// grabStore durably remembers, per torrent hash, which indexer grabbed it
// — the one piece of seedarr's join the arrs do NOT keep around forever.
// The arr's DELETE .../movie/{id}?deleteFiles=true call cascade-deletes
// the movie's own history along with it, not just the file — so a torrent
// that was perfectly traceable one moment can become completely
// untraceable via the arr's own APIs the next, with no way to tell "never
// grabbed by an arr" apart from
// "was grabbed, but the arr forgot." Without this store, that torrent's
// share limits just freeze forever at whatever they last were — the whole
// point of resetting them to the tracker's floor once a title is gone
// never happens.
//
// Each row records which arr saw the grab and that arr's own local id for
// the indexer. That pair is the whole identity: an arr-local id means
// nothing without knowing which arr it came from (the same number is a
// different tracker in each), hence source alongside it. Nothing
// cross-arr is stored — a grab only ever needs to resolve against the arr
// that made it, so there's no reason to know that two local ids happen to
// be the same tracker.
//
// For Sonarr grabs the local id is worked out at capture time from the
// Prowlarr id in the grab's downloadUrl (see arr.go's grabsFrom); that
// Prowlarr id is a matching aid used once and never persisted.
//
// Populated by its own background job (a reset only reads from this
// store, it never writes to it), specifically so the write happens well
// before any deletion could ever cascade the source data away.
//
// Deliberately minimal: only the indexer id is stored, never the seed
// ratio/time itself — the arrs' own live indexer config is the single
// source of truth for those (Prowlarr pushes changes into it live) and
// can change after the grab, so this store only
// needs to answer "which indexer", not "what did it require back then".
type grabStore struct {
	db *sql.DB
}

func openGrabStore(path string) (*grabStore, error) {
	// _pragma=busy_timeout(5000): the trigger handler and the grab-events
	// job run concurrently in separate goroutines (see main.go) and both
	// open the same file — this collides without a busy timeout
	// (SQLITE_BUSY on a read during the grab-events job's write
	// transaction, failing the reset). journal_mode=WAL lets the job's
	// writes and the trigger's reads proceed concurrently instead of
	// blocking each other in the first place; the timeout remains as a
	// backstop for the moments WAL still needs to serialize (e.g. two
	// concurrent writers, which doesn't happen here today but costs
	// nothing to guard against).
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS grabs (
			hash TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			arr_indexer_id INTEGER NOT NULL,
			first_seen_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("could not create grabs table: %w", err)
	}

	store := &grabStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// migrate brings an older store up to the current schema. Two earlier
// shapes exist:
//
//   - The original, Radarr-only store had just arr_indexer_id, holding
//     Radarr's own local id. Those rows are already correct for the column
//     of the same name and only need source filled in — necessarily
//     "radarr", the only arr that existed.
//   - An interim schema also carried a prowlarr_indexer_id column, from a
//     design that keyed everything on Prowlarr's id. That column is dropped
//     outright: nothing reads it any more, and leaving a NOT NULL column
//     behind would make every insert that omits it fail.
//
// Interim Sonarr rows have arr_indexer_id = 0, since that design never
// resolved Sonarr's local id. They're unresolvable until the grab-events
// job re-captures them — which it does on its first run, for every hash
// Sonarr's history still has.
func (s *grabStore) migrate() error {
	columns, err := s.columns()
	if err != nil {
		return err
	}

	if _, ok := columns["source"]; !ok {
		if _, hasLegacy := columns["arr_indexer_id"]; hasLegacy {
			if _, err := s.db.Exec(`ALTER TABLE grabs ADD COLUMN source TEXT NOT NULL DEFAULT 'radarr'`); err != nil {
				return fmt.Errorf("could not add the source column: %w", err)
			}
		}
	}

	if _, ok := columns["prowlarr_indexer_id"]; ok {
		if _, err := s.db.Exec(`ALTER TABLE grabs DROP COLUMN prowlarr_indexer_id`); err != nil {
			return fmt.Errorf("could not drop the prowlarr_indexer_id column: %w", err)
		}
	}
	return nil
}

func (s *grabStore) columns() (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('grabs')`)
	if err != nil {
		return nil, fmt.Errorf("could not inspect the grabs table: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = struct{}{}
	}
	return columns, rows.Err()
}

func (s *grabStore) close() error {
	return s.db.Close()
}

// grab is one remembered grab: which arr saw it, and that arr's own local
// id for the indexer it came through. Together they name exactly one
// indexer in exactly one arr, which is all a reset needs to look its
// requirement up.
type grab struct {
	source       string // "radarr" or "sonarr" — which arr's history this came from
	arrIndexerID int    // that arr's OWN local indexer id, meaningful only alongside source
}

// upsertGrab is the one statement that writes a grab row. The row is
// updated on conflict (rather than left as first-seen) in case a hash was
// ever re-grabbed via a different indexer — the most recent grab is the
// one that actually matters, and it may well have come through the other
// arr entirely.
const upsertGrab = `
	INSERT INTO grabs (hash, source, arr_indexer_id) VALUES (?, ?, ?)
	ON CONFLICT(hash) DO UPDATE SET
		source = excluded.source,
		arr_indexer_id = excluded.arr_indexer_id
`

// remember upserts a single hash's grab.
func (s *grabStore) remember(hash string, g grab) error {
	_, err := s.db.Exec(upsertGrab, hash, g.source, g.arrIndexerID)
	return err
}

// rememberAll upserts every grab the grab-events job just captured, in one
// transaction — the job re-remembers everything both arrs can still see on
// every run, so this is the hot path, and the row count only ever grows.
//
// One transaction rather than a row at a time because each bare Exec is
// its own implicit transaction, and SQLite's durability cost is per
// commit, not per row — so a loop pays that cost once per row for what is
// logically one update. It also makes the run atomic: a reset reading
// concurrently (see openGrabStore's WAL note) sees either the previous
// run's rows or this one's, never a half-applied mixture.
//
// Returns the number written. A failure rolls the whole batch back and the
// next run simply re-captures it — the job's own retry, not something to
// patch up halfway.
func (s *grabStore) rememberAll(grabs map[string]grab) (int, error) {
	if len(grabs) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("could not begin the grab transaction: %w", err)
	}
	defer tx.Rollback() // no-op once committed

	stmt, err := tx.Prepare(upsertGrab)
	if err != nil {
		return 0, fmt.Errorf("could not prepare the grab upsert: %w", err)
	}
	defer stmt.Close()

	for hash, g := range grabs {
		if _, err := stmt.Exec(hash, g.source, g.arrIndexerID); err != nil {
			return 0, fmt.Errorf("could not remember the grab for hash %s: %w", hash, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("could not commit the grab transaction: %w", err)
	}
	return len(grabs), nil
}

// grabFor looks up which indexer, in which arr, a previously-remembered
// hash was grabbed through.
//
// ok=false means seedarr has never seen this hash grabbed by either arr
// at all — distinct from "grabbed but the arr has since forgotten it",
// which is exactly the case this store exists to cover. A row whose
// arr_indexer_id is 0 (an interim-schema Sonarr row not yet re-captured;
// see migrate) is reported the same way, since no arr has an indexer 0.
func (s *grabStore) grabFor(hash string) (g grab, ok bool, err error) {
	row := s.db.QueryRow(`SELECT source, arr_indexer_id FROM grabs WHERE hash = ?`, hash)
	err = row.Scan(&g.source, &g.arrIndexerID)
	if err == sql.ErrNoRows {
		return grab{}, false, nil
	}
	if err != nil {
		return grab{}, false, err
	}
	if g.arrIndexerID == 0 {
		return grab{}, false, nil
	}
	return g, true, nil
}
