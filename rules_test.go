package main

import "testing"

func TestResolveTrackerFloor_TwoIndexers_UnconfiguredOneFallsBackToGlobalDefault(t *testing.T) {
	cases := []struct {
		name      string
		criteria  prowlarrSeedFields
		wantFloor trackerFloor
	}{
		{
			name:      "a private tracker with a real ratio+time requirement configured",
			criteria:  prowlarrSeedFields{ratio: 1.1, ratioOK: true, minutes: 3500, timeOK: true},
			wantFloor: trackerFloor{ratio: 1.1, minutes: 3500},
		},
		{
			name:      "a public tracker with neither half set — falls back to qbittorrent's global default",
			criteria:  prowlarrSeedFields{},
			wantFloor: trackerFloor{ratio: useGlobalDefault, minutes: int64(useGlobalDefault)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveTrackerFloor(c.criteria)
			if got != c.wantFloor {
				t.Fatalf("resolveTrackerFloor(%+v) = %+v, want %+v", c.criteria, got, c.wantFloor)
			}
		})
	}
}

func TestResolveTrackerFloor_PartiallyConfigured_OnlyTheUnsetHalfFallsBackToGlobalDefault(t *testing.T) {
	// Each half resolves on its own: the configured one is applied as the
	// tracker requires it, and only the unset one falls back. Both
	// directions are covered, since a tracker can leave either half unset
	// — a seed time with no ratio is a real, common configuration.
	cases := []struct {
		name      string
		criteria  prowlarrSeedFields
		wantFloor trackerFloor
	}{
		{
			name:      "a seed time but no ratio — the real seed time still applies",
			criteria:  prowlarrSeedFields{minutes: 10080, timeOK: true},
			wantFloor: trackerFloor{ratio: useGlobalDefault, minutes: 10080},
		},
		{
			name:      "a ratio with no seed time — the real ratio still applies",
			criteria:  prowlarrSeedFields{ratio: 2.0, ratioOK: true},
			wantFloor: trackerFloor{ratio: 2.0, minutes: int64(useGlobalDefault)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveTrackerFloor(c.criteria)
			if got != c.wantFloor {
				t.Fatalf("resolveTrackerFloor(%+v) = %+v, want %+v", c.criteria, got, c.wantFloor)
			}
		})
	}
}
