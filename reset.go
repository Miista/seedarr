package main

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog"
)

// resetter holds what a reset needs — qBittorrent, the arrs and the grab
// store — and does the one thing seedarr does: bring a torrent's share
// limits to its tracker's requirement. It only ever READS the grab store
// (see store.go); writing to it is the grab-events job's job (see
// jobs.go), running on its own schedule so a grab is captured well before
// any deletion could cascade it away.
//
// There is deliberately no polling loop. seedarr cannot know on its own
// which torrents have left the library — hardlink state is qui's
// knowledge, not seedarr's — so every reset is driven by qui calling the
// HTTP trigger (see trigger.go). Retry is qui's as well: its automation
// re-evaluates on an interval and only matches torrents still at
// unlimited ratio, so a reset that fails simply matches again next pass.
// Nothing here needs to remember what's been handled.
type resetter struct {
	qbittorrent *qbittorrentClient
	arr         *arrClient
	store       *grabStore
	log         zerolog.Logger
}

// outcome is what applyFloor did about one torrent, so the trigger can
// answer the caller in those terms (see trigger.go).
type outcome int

const (
	// outcomeNotOurs means nothing was done because no arr ever grabbed
	// this torrent: a book from Shelfarr, a manual add. Not a failure —
	// there is simply no tracker requirement to resolve.
	outcomeNotOurs outcome = iota
	// outcomeChanged means the limits differed from the tracker's floor
	// and were updated.
	outcomeChanged
	// outcomeAlreadyCorrect means the limits already matched the floor and
	// nothing was written — a repeat call, or one that raced an earlier
	// reset. Success, not an error.
	outcomeAlreadyCorrect
)

// applyFloor is the whole per-torrent decision: look up which arr grabbed
// this hash and through which indexer, read that indexer's required
// ratio/seed time from that same arr's list (or qBittorrent's global
// default for whichever half the tracker leaves unset), and apply it if
// it differs from what's already set.
func (r *resetter) applyFloor(t torrent, indexers indexersBySource) (outcome, error) {
	g, known, err := r.store.grabFor(strings.ToLower(t.Hash))
	if err != nil {
		return outcomeNotOurs, fmt.Errorf("could not look up the stored indexer: %w", err)
	}
	if !known {
		return outcomeNotOurs, nil // never grabbed by an arr — not a candidate at all
	}

	// An indexer removed from its arr since the grab leaves nothing to
	// read — lookup's zero value has no criteria, so the floor falls back
	// to qBittorrent's global default on both halves.
	idx, _ := indexers.lookup(g)

	floor := resolveTrackerFloor(idx.criteria)
	if t.RatioLimit == floor.ratio && t.SeedingTimeLimit == floor.minutes {
		return outcomeAlreadyCorrect, nil
	}

	r.log.Info().Msg(fmt.Sprintf("'%s': setting ratio %.2f / %dm", t.Name, floor.ratio, floor.minutes))
	if err := r.qbittorrent.setShareLimits(t.Hash, floor.ratio, floor.minutes); err != nil {
		return outcomeNotOurs, fmt.Errorf("could not update share limits: %w", err)
	}
	return outcomeChanged, nil
}
