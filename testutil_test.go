package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// testLogger discards output by default so `go test` stays quiet; run with
// `go test -v` to see log lines interleaved with test output (useful when
// debugging a failing case).
func testLogger(t *testing.T) zerolog.Logger {
	t.Helper()
	var w io.Writer = io.Discard
	if testing.Verbose() {
		w = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}
	}
	return zerolog.New(w).Level(zerolog.DebugLevel).With().Timestamp().Logger()
}

// fakeQbittorrentConfig describes everything a test's fake qBittorrent
// server needs to serve: the torrents it knows about, and which endpoints
// (if any) should fail — so the trigger's error-handling branches can be
// exercised against a real (faked) HTTP failure, not a mocked error
// return.
type fakeQbittorrentConfig struct {
	torrents []torrent

	failLogin              bool
	failTorrents           bool            // /torrents/info returns 500
	failShareLimitsForHash map[string]bool // hash -> /torrents/setShareLimits returns 500 for this hash
}

// shareLimitCall records one setShareLimits call's arguments, so a test can
// assert not just "was it called" but "was it called with the right
// values" — read back afterward from the fake server's own state.
type shareLimitCall struct {
	ratio   float64
	minutes int64
}

// fakeCalls wraps the recording map newFakeQbittorrent returns, with the
// type assertion handled once rather than at every assertion.
type fakeCalls struct {
	calls *sync.Map // hash -> shareLimitCall
}

func (f *fakeCalls) shareLimits(hash string) (shareLimitCall, bool) {
	v, ok := f.calls.Load(hash)
	if !ok {
		return shareLimitCall{}, false
	}
	return v.(shareLimitCall), true
}

// newFakeQbittorrent fakes qBittorrent's real HTTP API (an external
// dependency) and returns the client plus a map recording every
// setShareLimits call it received, keyed by hash.
func newFakeQbittorrent(t *testing.T, cfg fakeQbittorrentConfig) (*qbittorrentClient, *sync.Map) {
	t.Helper()
	var calls sync.Map // hash -> shareLimitCall

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/auth/login":
			if cfg.failLogin {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// Recent qBittorrent versions return 204 No Content on a
			// successful login, not 200 — the fake deliberately matches
			// that quirk rather than the more commonly documented 200, so a
			// regression back to "only 200 is accepted" fails every test
			// here, not just a dedicated one.
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/api/v2/torrents/info":
			if cfg.failTorrents {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Real qBittorrent filters /torrents/info by the ?hashes=
			// parameter, matching case-insensitively — the fake does the
			// same so torrentByHash is genuinely exercised rather than
			// being handed the whole list and appearing to work.
			wanted := r.URL.Query().Get("hashes")
			if wanted == "" {
				json.NewEncoder(w).Encode(cfg.torrents)
				return
			}
			matched := []torrent{}
			for _, t := range cfg.torrents {
				if strings.EqualFold(t.Hash, wanted) {
					matched = append(matched, t)
				}
			}
			json.NewEncoder(w).Encode(matched)

		case r.URL.Path == "/api/v2/torrents/setShareLimits":
			_ = r.ParseForm()
			hash := r.Form.Get("hashes")
			if cfg.failShareLimitsForHash[hash] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// qBittorrent 5.2+ rejects this call with 400 "Missing required
			// parameters: shareLimitAction" if it's absent — the fake
			// enforces the same requirement so a regression back to
			// omitting it fails every share-limit test here, not just a
			// dedicated one.
			if r.Form.Get("shareLimitAction") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			ratio, _ := strconv.ParseFloat(r.Form.Get("ratioLimit"), 64)
			minutes, _ := strconv.ParseInt(r.Form.Get("seedingTimeLimit"), 10, 64)
			calls.Store(hash, shareLimitCall{ratio: ratio, minutes: minutes})
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := newQbittorrentClient(srv.URL, "user", "pass", testLogger(t))
	if err != nil {
		t.Fatalf("newQbittorrentClient: %v", err)
	}
	client.httpClient = srv.Client()
	return client, &calls
}

// fakeProwlarrURL builds the URL Prowlarr generates for one of its own
// indexers — the shape both an arr's indexer baseUrl and a history
// record's downloadUrl take ("http://prowlarr:9696/17/download?apikey=...").
func fakeProwlarrURL(prowlarrID int, path string) string {
	return "http://prowlarr:9696/" + strconv.Itoa(prowlarrID) + "/" + path
}

// fakeArrIndexer describes one entry in an arr's own /api/v3/indexer list.
// arrID is that arr's LOCAL id and prowlarrID is the one in its baseUrl —
// kept deliberately distinct in tests (never equal) so that storing or
// looking up the wrong one fails outright: one tracker can be Radarr-local
// 10 / Sonarr-local 12 / Prowlarr 17 all at once, and only the local id,
// paired with its arr, is ever meant to be kept.
type fakeArrIndexer struct {
	arrID      int
	prowlarrID int
	ratio      *float64 // nil means "not configured" — the real, common case for public trackers
	minutes    *int64
}

// fakeGrab describes one grab event. Which fields are populated decides
// which resolution path the grab exercises:
//
//   - arrID set: history carries data.indexerId, the arr's own local id —
//     Radarr's shape (populated on every grab).
//   - arrID zero: no indexerId at all, so resolution must fall back to the
//     Prowlarr id embedded in downloadUrl — Sonarr's shape (indexerId is
//     null on every grab, downloadUrl is always set).
//
// prowlarrID always populates downloadUrl, since the real APIs always set
// it on both arrs.
type fakeGrab struct {
	hash       string
	arrID      int
	prowlarrID int
}

// newFakeArr fakes one arr's real HTTP API (an external dependency): both
// /api/v3/indexer (shape: local id, plus a field bag carrying baseUrl and
// seedCriteria.*) and /api/v3/history (shape:
// downloadId/eventType/data.indexerId/data.downloadUrl).
func newFakeArr(t *testing.T, indexers []fakeArrIndexer, grabs []fakeGrab) string {
	t.Helper()

	var indexerPayload []map[string]any
	for _, idx := range indexers {
		var ratioVal, minutesVal any
		if idx.ratio != nil {
			ratioVal = *idx.ratio
		}
		if idx.minutes != nil {
			minutesVal = float64(*idx.minutes)
		}
		fields := []map[string]any{
			{"name": "baseUrl", "value": fakeProwlarrURL(idx.prowlarrID, "")},
			{"name": "seedCriteria.seedRatio", "value": ratioVal},
			{"name": "seedCriteria.seedTime", "value": minutesVal},
		}
		indexerPayload = append(indexerPayload, map[string]any{"id": idx.arrID, "fields": fields})
	}

	var historyRecords []map[string]any
	for _, g := range grabs {
		data := map[string]any{
			"downloadUrl": fakeProwlarrURL(g.prowlarrID, "download?apikey=k&file=x"),
		}
		// Sonarr omits indexerId entirely rather than sending an empty
		// string — the fake matches that, so the fallback path is
		// exercised against the real absent-field shape.
		if g.arrID != 0 {
			data["indexerId"] = strconv.Itoa(g.arrID)
		}
		historyRecords = append(historyRecords, map[string]any{
			"downloadId": g.hash,
			"eventType":  "grabbed",
			"data":       data,
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/indexer":
			json.NewEncoder(w).Encode(indexerPayload)
		case "/api/v3/history":
			page := r.URL.Query().Get("page")
			if page != "1" {
				json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"records": historyRecords})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newFakeRadarr builds an arrClient with only Radarr configured — the
// common case for tests that aren't specifically about Sonarr.
func newFakeRadarr(t *testing.T, indexers []fakeArrIndexer, grabs []fakeGrab) *arrClient {
	t.Helper()
	return &arrClient{
		radarrURL:    newFakeArr(t, indexers, grabs),
		radarrAPIKey: "radarr-key",
		httpClient:   http.DefaultClient,
		log:          testLogger(t),
	}
}

// newFakeRadarrAndSonarr builds an arrClient with both arrs configured,
// each with its own indexer list and history — the typical shape, where
// the same tracker appears in both under different local ids.
func newFakeRadarrAndSonarr(t *testing.T, radarrIndexers []fakeArrIndexer, radarrGrabs []fakeGrab, sonarrIndexers []fakeArrIndexer, sonarrGrabs []fakeGrab) *arrClient {
	t.Helper()
	return &arrClient{
		radarrURL:    newFakeArr(t, radarrIndexers, radarrGrabs),
		radarrAPIKey: "radarr-key",
		sonarrURL:    newFakeArr(t, sonarrIndexers, sonarrGrabs),
		sonarrAPIKey: "sonarr-key",
		httpClient:   http.DefaultClient,
		log:          testLogger(t),
	}
}

// newUnreachableArr builds an arrClient whose Radarr answers every request
// with a 500 — for exercising the "arr unavailable" path against a real
// (faked) HTTP failure.
func newUnreachableArr(t *testing.T) *arrClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return &arrClient{radarrURL: srv.URL, radarrAPIKey: "key", httpClient: srv.Client(), log: testLogger(t)}
}

func floatPtr(f float64) *float64 { return &f }
func int64Ptr(i int64) *int64     { return &i }

// newTestStore opens a real SQLite file in a fresh temp directory — the
// store is seedarr's own persistent state, a genuine external dependency
// (a real file on disk), not something to fake.
func newTestStore(t *testing.T) *grabStore {
	t.Helper()
	store, err := openGrabStore(filepath.Join(t.TempDir(), "seedarr.db"))
	if err != nil {
		t.Fatalf("openGrabStore: %v", err)
	}
	t.Cleanup(func() { store.close() })
	return store
}

func newResetter(t *testing.T, qbt *qbittorrentClient, arr *arrClient) *resetter {
	t.Helper()
	return &resetter{
		qbittorrent: qbt,
		arr:         arr,
		store:       newTestStore(t),
		log:         testLogger(t),
	}
}

// primeStore seeds the store as if an earlier grab-events job run had
// already remembered these hash -> grab mappings — the same thing a reset
// reads, but populated directly here since a reset never writes to the
// store itself (that's the grab-events job's job).
func primeStore(t *testing.T, r *resetter, grabs map[string]grab) {
	t.Helper()
	for hash, g := range grabs {
		if err := r.store.remember(hash, g); err != nil {
			t.Fatalf("store.remember(%q, %+v): %v", hash, g, err)
		}
	}
}

// radarrGrab is the common test case: a torrent Radarr grabbed through
// its local indexer id.
func radarrGrab(hash string, arrIndexerID int) map[string]grab {
	return map[string]grab{hash: {source: "radarr", arrIndexerID: arrIndexerID}}
}
