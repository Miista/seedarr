package main

// qBittorrent's own sentinel value for setShareLimits meaning "use the
// globally configured default ratio/seeding time" — reused directly rather
// than inventing a seedarr-specific constant, since it crosses straight
// through to the qBittorrent API call.
const useGlobalDefault float64 = -2

// trackerFloor is the target ratio/seeding-time-minutes to apply to a
// torrent whose library copy is gone.
type trackerFloor struct {
	ratio   float64
	minutes int64
}

// resolveTrackerFloor reads criteria (the indexer's own
// seedCriteria.seedRatio/seedTime, as reported by the arr's indexer list)
// and returns the floor to apply.
//
// Each half is resolved independently: a configured value is applied as
// the tracker requires it, and an unconfigured one falls back to
// qBittorrent's global default. Independently, because trackers really do
// configure them that way — one may require a week of seeding and no
// ratio at all, which should apply that real seed time rather than
// discarding it just because its other half is unset. There is always
// something to apply once a torrent has left the library, never a reason
// to leave its limits untouched.
func resolveTrackerFloor(criteria prowlarrSeedFields) trackerFloor {
	floor := trackerFloor{ratio: useGlobalDefault, minutes: int64(useGlobalDefault)}
	if criteria.ratioOK {
		floor.ratio = criteria.ratio
	}
	if criteria.timeOK {
		floor.minutes = criteria.minutes
	}
	return floor
}
