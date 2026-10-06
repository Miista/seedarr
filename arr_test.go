package main

import "testing"

func TestProwlarrIDFromURL_RealShapes(t *testing.T) {
	// Both shapes Prowlarr actually generates: the
	// baseUrl it syncs into each arr's indexer entry, and the per-grab
	// downloadUrl it records in history.
	cases := []struct {
		name   string
		url    string
		want   int
		wantOK bool
	}{
		{name: "indexer baseUrl", url: "http://prowlarr:9696/17/", want: 17, wantOK: true},
		{name: "baseUrl without trailing slash", url: "http://prowlarr:9696/17", want: 17, wantOK: true},
		{name: "history downloadUrl", url: "http://prowlarr:9696/17/download?apikey=k&link=abc&file=x", want: 17, wantOK: true},
		{name: "https and a non-default host", url: "https://prowlarr.example.com/4/download?apikey=k", want: 4, wantOK: true},
		{name: "an indexer added by hand in the arr, not synced from prowlarr", url: "https://tracker.example.org/api", wantOK: false},
		{name: "empty", url: "", wantOK: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := prowlarrIDFromURL(c.url)
			if ok != c.wantOK || (c.wantOK && got != c.want) {
				t.Fatalf("prowlarrIDFromURL(%q) = (%d, %v), want (%d, %v)", c.url, got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestResolveLocalIndexerID_IndexerIDPresent_UsedDirectly(t *testing.T) {
	// Radarr's shape: indexerId is present, so the arr's own statement of
	// which indexer it used wins. The downloadUrl deliberately carries a
	// Prowlarr id that maps to a DIFFERENT local id, so a regression that
	// reaches for the URL first fails rather than passing by coincidence.
	prowlarrToLocal := map[int]int{999: 55}
	r := historyRecord{
		Data: historyRecordData{
			IndexerID:   "10",
			DownloadURL: "http://prowlarr:9696/999/download?apikey=k",
		},
	}

	got, ok := resolveLocalIndexerID(r, prowlarrToLocal)
	if !ok || got != 10 {
		t.Fatalf("resolveLocalIndexerID = (%d, %v), want (10, true)", got, ok)
	}
}

func TestResolveLocalIndexerID_NoIndexerID_MatchedViaProwlarrIDInURL(t *testing.T) {
	// Sonarr's shape: no indexerId at all (it's null on every Sonarr grab).
	// The Prowlarr id in the downloadUrl (17) is matched to the Sonarr
	// indexer whose baseUrl carries the same id, and THAT indexer's local
	// id (12) is the answer — the Prowlarr id itself is never returned.
	r := historyRecord{
		Data: historyRecordData{
			DownloadURL: "http://prowlarr:9696/17/download?apikey=k&file=Some.Show.S01E01",
		},
	}

	got, ok := resolveLocalIndexerID(r, map[int]int{17: 12})
	if !ok || got != 12 {
		t.Fatalf("resolveLocalIndexerID = (%d, %v), want (12, true)", got, ok)
	}
}

func TestResolveLocalIndexerID_ProwlarrIDNotInSonarr_NotGuessedAt(t *testing.T) {
	// The URL names a Prowlarr indexer Sonarr doesn't have — removed since
	// the grab, or never synced. There's no local indexer to attribute the
	// grab to, so it's skipped rather than guessed.
	r := historyRecord{Data: historyRecordData{DownloadURL: "http://prowlarr:9696/17/download?apikey=k"}}
	if _, ok := resolveLocalIndexerID(r, map[int]int{4: 3}); ok {
		t.Fatalf("expected a grab whose Prowlarr id matches no local indexer to be skipped")
	}
}

func TestResolveLocalIndexerID_HandAddedIndexer_NotGuessedAt(t *testing.T) {
	// A Sonarr grab through an indexer added directly in Sonarr, not via
	// Prowlarr: no indexerId, and a downloadUrl pointing straight at the
	// tracker with no Prowlarr id in it. Out of scope, by design.
	r := historyRecord{Data: historyRecordData{DownloadURL: "https://tracker.example.org/direct.torrent"}}
	if _, ok := resolveLocalIndexerID(r, map[int]int{17: 12}); ok {
		t.Fatalf("expected a grab resolvable through neither path to be skipped, not guessed at")
	}
}

func TestIndexers_BothArrs_KeptSeparatePerArr(t *testing.T) {
	// The same local id means a different tracker in each arr: local 14 is
	// PublicB in Radarr but PublicC in Sonarr. The map must keep the two
	// apart rather than merging on the number — and must NOT merge the
	// shared tracker (Radarr-local 10 / Sonarr-local 12) either, since each
	// grab only ever looks up its own arr.
	arr := newFakeRadarrAndSonarr(t,
		[]fakeArrIndexer{
			{arrID: 10, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}, // AlphaTracker
			{arrID: 14, prowlarrID: 22, ratio: nil, minutes: nil},                      // PublicB
		},
		nil,
		[]fakeArrIndexer{
			{arrID: 12, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}, // AlphaTracker again
			{arrID: 14, prowlarrID: 19, ratio: floatPtr(2.0), minutes: int64Ptr(60)},   // PublicC — distinct criteria so a merge is detectable
		},
		nil,
	)

	indexers, err := arr.indexers()
	if err != nil {
		t.Fatalf("indexers: %v", err)
	}

	if got := indexers.count(); got != 4 {
		t.Fatalf("expected 4 indexers (2 per arr, none merged), got %d", got)
	}
	if idx, ok := indexers.lookup(grab{source: "radarr", arrIndexerID: 10}); !ok || idx.criteria.ratio != 1.1 {
		t.Fatalf("radarr local 10 = %+v (ok=%v), want AlphaTracker's 1.1", idx.criteria, ok)
	}
	if idx, ok := indexers.lookup(grab{source: "sonarr", arrIndexerID: 12}); !ok || idx.criteria.ratio != 1.1 {
		t.Fatalf("sonarr local 12 = %+v (ok=%v), want AlphaTracker's 1.1", idx.criteria, ok)
	}
	// Local 14 in each arr must resolve to ITS OWN tracker's criteria.
	if idx, ok := indexers.lookup(grab{source: "radarr", arrIndexerID: 14}); !ok || idx.criteria.ratioOK {
		t.Fatalf("radarr local 14 = %+v (ok=%v), want PublicB with nothing configured", idx.criteria, ok)
	}
	if idx, ok := indexers.lookup(grab{source: "sonarr", arrIndexerID: 14}); !ok || idx.criteria.ratio != 2.0 {
		t.Fatalf("sonarr local 14 = %+v (ok=%v), want PublicC's 2.0 — a cross-arr merge would have clobbered it", idx.criteria, ok)
	}
}

func TestIndexersLookup_IndexerRemovedFromArr_NotFound(t *testing.T) {
	arr := newFakeRadarr(t, []fakeArrIndexer{{arrID: 10, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}}, nil)
	indexers, err := arr.indexers()
	if err != nil {
		t.Fatalf("indexers: %v", err)
	}
	if _, ok := indexers.lookup(grab{source: "radarr", arrIndexerID: 99}); ok {
		t.Fatalf("expected a local id the arr no longer has to be reported as not found")
	}
	if _, ok := indexers.lookup(grab{source: "sonarr", arrIndexerID: 10}); ok {
		t.Fatalf("expected a lookup against an unconfigured arr to be reported as not found")
	}
}

func TestGrabbedIndexerIDs_BothArrs_EachResolvesToItsOwnLocalID(t *testing.T) {
	// A movie grabbed via Radarr (indexerId present) and an episode grabbed
	// via Sonarr (no indexerId, downloadUrl only), both through the same
	// tracker. Each resolves to ITS ARR's local id for that tracker — 10 in
	// Radarr, 12 in Sonarr — with no Prowlarr id stored for either.
	arr := newFakeRadarrAndSonarr(t,
		[]fakeArrIndexer{{arrID: 10, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}},
		[]fakeGrab{{hash: "MOVIEHASH", arrID: 10, prowlarrID: 17}},
		[]fakeArrIndexer{{arrID: 12, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}},
		[]fakeGrab{{hash: "EPISODEHASH", prowlarrID: 17}}, // arrID omitted: sonarr's real shape
	)

	byHash, err := arr.grabbedIndexerIDs()
	if err != nil {
		t.Fatalf("grabbedIndexerIDs: %v", err)
	}

	wantMovie := grab{source: "radarr", arrIndexerID: 10}
	if got := byHash["moviehash"]; got != wantMovie {
		t.Fatalf("radarr grab = %+v, want %+v", got, wantMovie)
	}
	// The Sonarr grab's local id (12) came from matching the URL's Prowlarr
	// id (17) against Sonarr's indexer list — Sonarr never reported it.
	wantEpisode := grab{source: "sonarr", arrIndexerID: 12}
	if got := byHash["episodehash"]; got != wantEpisode {
		t.Fatalf("sonarr grab = %+v, want %+v", got, wantEpisode)
	}
}

func TestGrabbedIndexerIDs_SonarrOnly_StillResolves(t *testing.T) {
	// A TV-only deployment: no Radarr configured at all. Sonarr's grabs
	// must still resolve, which is the whole point — before this, they
	// resolved to nothing and their torrents were silently never touched.
	arr := newFakeRadarrAndSonarr(t, nil, nil,
		[]fakeArrIndexer{{arrID: 17, prowlarrID: 23, ratio: floatPtr(1.0), minutes: int64Ptr(5760)}},
		[]fakeGrab{{hash: "EPISODEHASH", prowlarrID: 23}},
	)
	arr.radarrAPIKey = "" // unconfigured

	byHash, err := arr.grabbedIndexerIDs()
	if err != nil {
		t.Fatalf("grabbedIndexerIDs: %v", err)
	}
	want := grab{source: "sonarr", arrIndexerID: 17}
	if got := byHash["episodehash"]; got != want {
		t.Fatalf("sonarr grab = %+v, want %+v", got, want)
	}
}

func TestGrabbedIndexerIDs_OneArrDown_TheOtherStillCaptured(t *testing.T) {
	// Capture is time-sensitive: history is cascade-deleted when a title
	// is removed, so a grab missed now can be missed for good. A Sonarr
	// outage must not cost us Radarr's grabs.
	arr := newFakeRadarrAndSonarr(t,
		[]fakeArrIndexer{{arrID: 10, prowlarrID: 17}},
		[]fakeGrab{{hash: "MOVIEHASH", arrID: 10, prowlarrID: 17}},
		nil, nil,
	)
	arr.sonarrURL = "http://127.0.0.1:1" // nothing listening

	byHash, err := arr.grabbedIndexerIDs()
	if err != nil {
		t.Fatalf("expected a down sonarr to be survivable, got %v", err)
	}
	want := grab{source: "radarr", arrIndexerID: 10}
	if got := byHash["moviehash"]; got != want {
		t.Fatalf("radarr grab = %+v, want %+v", got, want)
	}
	if len(byHash) != 1 {
		t.Fatalf("expected only radarr's grab, got %d", len(byHash))
	}
}

func TestGrabbedIndexerIDs_EveryArrDown_IsAnError(t *testing.T) {
	// With nothing reachable there is genuinely nothing to record, so the
	// job must log a failure rather than quietly report zero grabs — which
	// would be indistinguishable from "the arrs have no history".
	arr := &arrClient{
		radarrURL: "http://127.0.0.1:1", radarrAPIKey: "k",
		sonarrURL: "http://127.0.0.1:1", sonarrAPIKey: "k",
		httpClient: newFakeRadarr(t, nil, nil).httpClient,
		log:        testLogger(t),
	}

	if _, err := arr.grabbedIndexerIDs(); err == nil {
		t.Fatalf("expected an error when no arr could be reached at all")
	}
}

func TestIndexers_OneArrDown_FailsRatherThanApplyingAPartialMap(t *testing.T) {
	// The opposite rule to grab capture: this map decides which floor to
	// APPLY, and a half-built one would hand torrents the global default
	// instead of their tracker's real requirement. Failing the reset is
	// right; writing wrong limits is not.
	arr := newFakeRadarrAndSonarr(t,
		[]fakeArrIndexer{{arrID: 10, prowlarrID: 17, ratio: floatPtr(1.1), minutes: int64Ptr(4320)}},
		nil, nil, nil,
	)
	arr.sonarrURL = "http://127.0.0.1:1"

	if _, err := arr.indexers(); err == nil {
		t.Fatalf("expected a down arr to fail the whole indexer fetch, not yield a partial map")
	}
}
