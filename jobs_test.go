package main

import "testing"

func TestGrabEventsJob_RunOnce_RemembersEveryGrabbedHash(t *testing.T) {
	arr := newFakeRadarr(t,
		[]fakeArrIndexer{{arrID: 9, prowlarrID: 16, ratio: floatPtr(1.1), minutes: int64Ptr(3500)}},
		[]fakeGrab{
			{hash: "H1", arrID: 9, prowlarrID: 16},
			{hash: "H2", arrID: 9, prowlarrID: 16},
		},
	)
	store := newTestStore(t)
	job := &grabEventsJob{arr: arr, store: store, log: testLogger(t)}

	job.runOnce()

	// Remembered under Radarr's own local id (9), not the Prowlarr id in
	// the fake's URLs (16) — the two are deliberately different numbers, so
	// a regression that stores the Prowlarr id fails outright rather than
	// passing by coincidence.
	for _, hash := range []string{"h1", "h2"} {
		g, known, err := store.grabFor(hash)
		if err != nil {
			t.Fatalf("grabFor(%q): %v", hash, err)
		}
		if !known {
			t.Fatalf("expected %q to be remembered in the store after runOnce", hash)
		}
		want := grab{source: "radarr", arrIndexerID: 9}
		if g != want {
			t.Fatalf("expected %q to be remembered as %+v, got %+v", hash, want, g)
		}
	}
}

func TestGrabEventsJob_RunOnce_NeverGrabbedHash_NotRemembered(t *testing.T) {
	arr := newFakeRadarr(t, []fakeArrIndexer{{arrID: 9, prowlarrID: 16, ratio: floatPtr(1.1), minutes: int64Ptr(3500)}}, nil)
	store := newTestStore(t)
	job := &grabEventsJob{arr: arr, store: store, log: testLogger(t)}

	job.runOnce()

	_, known, err := store.grabFor("h1")
	if err != nil {
		t.Fatalf("grabFor: %v", err)
	}
	if known {
		t.Fatalf("expected a hash never grabbed by radarr to NOT be remembered")
	}
}

func TestGrabEventsJob_RunOnce_PriorRunAlreadyRemembered_GetsOverwritten(t *testing.T) {
	// A hash the store already remembers (from an earlier run) under one
	// indexer should end up remembered under whatever runOnce sees THIS
	// time — exercised through runOnce itself, not store.remember
	// directly, so the job's actual write path is under test.
	arr := newFakeRadarr(t,
		[]fakeArrIndexer{{arrID: 25, prowlarrID: 33, ratio: floatPtr(1.0), minutes: int64Ptr(10080)}},
		[]fakeGrab{{hash: "H1", arrID: 25, prowlarrID: 33}},
	)
	store := newTestStore(t)
	job := &grabEventsJob{arr: arr, store: store, log: testLogger(t)}

	if err := store.remember("h1", grab{source: "radarr", arrIndexerID: 9}); err != nil {
		t.Fatalf("store.remember: %v", err)
	}

	job.runOnce()

	g, known, err := store.grabFor("h1")
	if err != nil {
		t.Fatalf("grabFor: %v", err)
	}
	want := grab{source: "radarr", arrIndexerID: 25}
	if !known || g != want {
		t.Fatalf("expected h1 to be updated to this run's indexer (%+v), got %+v known=%v", want, g, known)
	}
}
