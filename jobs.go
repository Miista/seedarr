package main

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// grabEventsJob runs on a cron schedule — the only thing in seedarr that
// does — and is the ONLY thing that ever writes into the grab store (see
// store.go).
// Its whole purpose is capturing each hash's grabbing indexer durably
// well before anything could delete the title and cascade that history
// away (the arr's deleteFiles=true deletion wipes the grab event too,
// not just the file). Each grab is recorded as the arr that made it plus
// that arr's own local indexer id (see arr.go).
type grabEventsJob struct {
	arr   *arrClient
	store *grabStore

	schedule cron.Schedule
	log      zerolog.Logger
}

func (j *grabEventsJob) run(ctx context.Context) {
	j.runOnce()

	now := time.Now()
	next := j.schedule.Next(now)
	timer := time.NewTimer(next.Sub(now))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			j.runOnce()
			next := j.schedule.Next(now)
			timer.Reset(time.Until(next))
		}
	}
}

func (j *grabEventsJob) runOnce() {
	j.log.Info().Msg("fetching grab events")

	byHash, err := j.arr.grabbedIndexerIDs()
	if err != nil {
		j.log.Error().Msg(fmt.Sprintf("could not fetch grab history, will try again next run: %v", err))
		return
	}
	j.log.Debug().Msg(fmt.Sprintf("the arrs report %d grabbed hashes", len(byHash)))

	// Written in one transaction: this is the same whole set every run, so
	// it's one logical update, and a partial write would leave the store
	// in a state no retry reasons about (see store.go's rememberAll).
	remembered, err := j.store.rememberAll(byHash)
	if err != nil {
		j.log.Error().Msg(fmt.Sprintf("could not remember this run's grabs, will try again next run: %v", err))
		return
	}

	j.log.Info().Msg(fmt.Sprintf("grab events finished: %d remembered", remembered))
}
