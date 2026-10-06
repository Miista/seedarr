package main

// prowlarrSeedFields is a tracker's seed ratio/time requirement. The
// values are Prowlarr's — it owns this config and pushes it into each
// arr's indexer entries live — but seedarr reads them from the arrs (see
// arr.go's arrIndexer.seedCriteria) rather than from Prowlarr's own API,
// since the arrs' copies are always current. Hence the name: these are
// Prowlarr's fields, just read second-hand.
type prowlarrSeedFields struct {
	ratio   float64
	ratioOK bool
	minutes int64
	timeOK  bool
}

// The two halves are tracked independently rather than as a single
// configured/not-configured pair, because trackers genuinely configure
// them independently — a seed-time requirement with no ratio requirement
// at all is a real, common configuration. Each half is resolved
// on its own (see resolveTrackerFloor); a missing half means "seedarr has
// no opinion on this one", which is precisely what qBittorrent's global
// default already expresses.
