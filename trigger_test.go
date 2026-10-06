package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestTrigger wires a trigger server onto a fake qBittorrent and the
// given arr client, with the store primed as if the grab-events job had
// already run, and returns it with the recording map so a test can assert
// what actually reached qBittorrent.
func newTestTrigger(t *testing.T, cfg fakeQbittorrentConfig, arr *arrClient, primed map[string]grab) (*triggerServer, *fakeCalls) {
	t.Helper()
	qbt, calls := newFakeQbittorrent(t, cfg)

	r := newResetter(t, qbt, arr)
	primeStore(t, r, primed)

	return &triggerServer{resetter: r, log: testLogger(t)}, &fakeCalls{calls: calls}
}

// post drives one request through the server's own route, so the path
// pattern and hash validation are exercised rather than bypassed by
// calling the handler directly.
func (s *triggerServer) post(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reset/{hash}", s.handleReset)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	return w
}

const (
	// A well-formed 40-hex v1 infohash — synthetic, not any real torrent.
	testHash      = "0123456789abcdef0123456789abcdef01234567"
	testHashUpper = "0123456789ABCDEF0123456789ABCDEF01234567"
)

// oneIndexer is the common single-indexer Radarr: local 20, with a real
// ratio+time requirement.
func oneIndexer(t *testing.T) *arrClient {
	t.Helper()
	return newFakeRadarr(t, []fakeArrIndexer{{arrID: 20, prowlarrID: 28, ratio: floatPtr(1.0), minutes: int64Ptr(4320)}}, nil)
}

func TestTriggerReset_AppliesTheTrackerFloor(t *testing.T) {
	// qui's whole reason for calling: the torrent is at unlimited and must
	// come back to its tracker's requirement.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Some.Movie.2026.1080p", RatioLimit: -1, SeedingTimeLimit: -1,
		}}},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	w := srv.post(t, "/reset/"+testHash)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, strings.TrimSpace(w.Body.String()))
	}
	got, called := fakes.shareLimits(testHash)
	if !called {
		t.Fatalf("expected setShareLimits to be called")
	}
	if got.ratio != 1.0 || got.minutes != 4320 {
		t.Fatalf("applied (%v, %v), want (1, 4320)", got.ratio, got.minutes)
	}
}

func TestTriggerReset_UppercaseHashFromQui_StillMatchesTheStore(t *testing.T) {
	// qui and the arrs report hashes uppercase; the store keys them
	// lowercase. The endpoint must normalise or it would never find a row.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHashUpper, Name: "Some.Movie.2026.1080p", RatioLimit: -1, SeedingTimeLimit: -1,
		}}},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	if w := srv.post(t, "/reset/"+testHashUpper); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if _, called := fakes.shareLimits(testHashUpper); !called {
		t.Fatalf("expected an uppercase hash to resolve against the lowercase store")
	}
}

func TestTriggerReset_AlreadyAtItsFloor_SucceedsWithoutRewriting(t *testing.T) {
	// qui may call for a torrent that's already correct — a repeat call,
	// or one that raced an earlier reset. That's success, not an error, and
	// it must not issue a redundant write.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Already Correct", RatioLimit: 1.0, SeedingTimeLimit: 4320,
		}}},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	w := srv.post(t, "/reset/"+testHash)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, called := fakes.shareLimits(testHash); called {
		t.Fatalf("expected no redundant setShareLimits call when already at the floor")
	}
}

func TestTriggerReset_UnconfiguredIndexer_FallsBackToGlobalDefault(t *testing.T) {
	// A public tracker: the arr has the indexer, but no seed ratio/time
	// configured. There's still something to apply — qBittorrent's own
	// global default on both halves.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Public Tracker", RatioLimit: 1.1, SeedingTimeLimit: 10080,
		}}},
		newFakeRadarr(t, []fakeArrIndexer{{arrID: 22, prowlarrID: 30, ratio: nil, minutes: nil}}, nil),
		radarrGrab(testHash, 22),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got, called := fakes.shareLimits(testHash)
	if !called {
		t.Fatalf("expected setShareLimits to be called even for an unconfigured indexer")
	}
	if got.ratio != useGlobalDefault || got.minutes != int64(useGlobalDefault) {
		t.Fatalf("expected the global default on both halves, got (%v, %v)", got.ratio, got.minutes)
	}
}

func TestTriggerReset_PartiallyConfiguredIndexer_UnsetHalfFallsBackToGlobalDefault(t *testing.T) {
	// Only the seed time is configured, ratio left unset — a real tracker
	// shape (a week of seeding, no ratio). The configured half must still
	// be applied; only the unset half falls back.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Half Configured", RatioLimit: 1.0, SeedingTimeLimit: 1.0,
		}}},
		newFakeRadarr(t, []fakeArrIndexer{{arrID: 22, prowlarrID: 30, ratio: nil, minutes: int64Ptr(10080)}}, nil),
		radarrGrab(testHash, 22),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got, called := fakes.shareLimits(testHash)
	if !called {
		t.Fatalf("expected setShareLimits to be called")
	}
	if got.ratio != useGlobalDefault || got.minutes != 10080 {
		t.Fatalf("expected the global default ratio with the tracker's real seed time, got (%v, %v)", got.ratio, got.minutes)
	}
}

func TestTriggerReset_IndexerRemovedFromArrSinceTheGrab_FallsBackToGlobalDefault(t *testing.T) {
	// The store remembers the grab came through local indexer 99, but the
	// arr no longer has it. Nothing to read a requirement from, so the
	// global default applies — a reset still happens rather than erroring,
	// since the torrent genuinely has left the library.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Orphaned Indexer", RatioLimit: -1, SeedingTimeLimit: -1,
		}}},
		oneIndexer(t), // has local 20 only
		radarrGrab(testHash, 99),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got, called := fakes.shareLimits(testHash)
	if !called {
		t.Fatalf("expected setShareLimits to be called with the global default")
	}
	if got.ratio != useGlobalDefault || got.minutes != int64(useGlobalDefault) {
		t.Fatalf("expected the global default, got (%v, %v)", got.ratio, got.minutes)
	}
}

func TestTriggerReset_SonarrGrabbedTorrent_ReadsItsOwnArrsList(t *testing.T) {
	// A TV torrent: its grab carried no indexerId, so at capture time it
	// was matched to Sonarr's local indexer 12 via the Prowlarr id in its
	// downloadUrl — and the reset must look that local id up in SONARR's
	// list. Radarr ALSO has a local 12, a different tracker entirely with
	// nothing configured — so a lookup that ignored the grab's source would
	// wrongly hand this torrent the global default instead of its real floor.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Some.Show.S01E01.1080p", RatioLimit: -1, SeedingTimeLimit: -1,
		}}},
		newFakeRadarrAndSonarr(t,
			[]fakeArrIndexer{{arrID: 12, prowlarrID: 4, ratio: nil, minutes: nil}},
			nil,
			[]fakeArrIndexer{{arrID: 12, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}},
			nil,
		),
		map[string]grab{testHash: {source: "sonarr", arrIndexerID: 12}},
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got, called := fakes.shareLimits(testHash)
	if !called {
		t.Fatalf("expected setShareLimits to be called for a sonarr-grabbed torrent")
	}
	if got.ratio != 1.1 || got.minutes != 4320 {
		t.Fatalf("expected sonarr's tracker floor (1.1, 4320), got (%v, %v) — read from the wrong arr?", got.ratio, got.minutes)
	}
}

func TestTriggerReset_NotGrabbedByAnArr_IsSuccessNotFailure(t *testing.T) {
	// A book from Shelfarr, or a manual add. seedarr has no requirement to
	// apply, which is a normal answer — a 5xx here would make qui's own
	// activity log noisy with failures that aren't failures.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Manually Added", RatioLimit: -1, SeedingTimeLimit: -1,
		}}},
		oneIndexer(t),
		nil, // nothing in the store
	)

	w := srv.post(t, "/reset/"+testHash)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a torrent seedarr doesn't manage", w.Code)
	}
	if _, called := fakes.shareLimits(testHash); called {
		t.Fatalf("expected no limits to be set for a torrent no arr ever grabbed")
	}
}

func TestTriggerReset_MalformedHash_Rejected(t *testing.T) {
	// The hash arrives from qui's own template as a URL path segment, so
	// it's validated rather than trusted.
	cases := []struct {
		name string
		path string
	}{
		{name: "too short", path: "/reset/abc123"},
		{name: "not hex", path: "/reset/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{name: "unsubstituted qui placeholder", path: "/reset/%7Bhash%7D"},
		{name: "sql-ish injection attempt", path: "/reset/" + testHash + "%27%20OR%201=1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, fakes := newTestTrigger(t,
				fakeQbittorrentConfig{torrents: []torrent{{Hash: testHash, Name: "Untouched"}}},
				oneIndexer(t),
				radarrGrab(testHash, 20),
			)

			w := srv.post(t, c.path)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if _, called := fakes.shareLimits(testHash); called {
				t.Fatalf("a malformed hash must not reach qbittorrent at all")
			}
		})
	}
}

func TestTriggerReset_UnknownToQbittorrent_Returns404(t *testing.T) {
	// Well-formed, but qBittorrent doesn't have it — already deleted, or
	// never there. Worth surfacing to qui rather than reporting success.
	srv, _ := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: nil},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// Every 500 below is a case qui will retry on its next pass, because the
// torrent's ratio is still unlimited and so still matches. That's the
// contract: a genuine failure must be a 5xx, never a quiet 200.

func TestTriggerReset_QbittorrentLoginFails_Returns500(t *testing.T) {
	srv, _ := newTestTrigger(t,
		fakeQbittorrentConfig{failLogin: true},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestTriggerReset_QbittorrentLookupFails_Returns500(t *testing.T) {
	// Login works but the torrent lookup itself errors — distinct from
	// "not found" (404): qBittorrent couldn't answer, so retry.
	srv, _ := newTestTrigger(t,
		fakeQbittorrentConfig{failTorrents: true},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestTriggerReset_ArrUnreachable_Returns500WithoutTouchingQbittorrent(t *testing.T) {
	// The indexer list decides which floor to APPLY; with the arr down
	// there's no safe answer, so fail — don't write a guessed limit.
	srv, fakes := newTestTrigger(t,
		fakeQbittorrentConfig{torrents: []torrent{{
			Hash: testHash, Name: "Some Movie", RatioLimit: -1, SeedingTimeLimit: -1,
		}}},
		newUnreachableArr(t),
		radarrGrab(testHash, 20),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if _, called := fakes.shareLimits(testHash); called {
		t.Fatalf("expected no share-limit write when the arr's indexer list is unreachable")
	}
}

func TestTriggerReset_SetShareLimitsFails_Returns500(t *testing.T) {
	srv, _ := newTestTrigger(t,
		fakeQbittorrentConfig{
			torrents:               []torrent{{Hash: testHash, Name: "Write Fails", RatioLimit: -1, SeedingTimeLimit: -1}},
			failShareLimitsForHash: map[string]bool{testHash: true},
		},
		oneIndexer(t),
		radarrGrab(testHash, 20),
	)

	if w := srv.post(t, "/reset/"+testHash); w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
