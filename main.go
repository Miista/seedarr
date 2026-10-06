// seedarr brings a torrent's qBittorrent share limits to its tracker's
// requirement once the torrent's data is no longer in the media library.
//
// It doesn't decide WHEN. qui's own hardlink automation knows when a
// torrent's library copy is gone, and calls seedarr's HTTP trigger for
// that one torrent as plain curl (see trigger.go) — no binary or store
// mounted into qui, no credentials duplicated there, no Docker socket.
// seedarr then applies the tracker's own required ratio/seed-time (falling
// back per-field to qBittorrent's global default where the tracker
// configures nothing). Retry is qui's too: its automation only matches
// torrents still at unlimited ratio, so a reset that fails simply matches
// again on qui's next pass. There is no polling loop in seedarr at all.
//
// Both Radarr and Sonarr are supported. A grab is identified by the arr
// that made it plus that arr's own local indexer id, and resolves against
// that arr alone — nothing is ever joined across the two. seedarr never
// calls Prowlarr; the one place Prowlarr enters is matching a Sonarr grab
// (which carries no indexer id) to its indexer via the Prowlarr id both
// embed in their URLs (see arr.go).
//
// The one thing seedarr does on a schedule is capture the arrs' grab
// events into a durable local store (see jobs.go/store.go), frequently,
// so a grab is recorded well before any deletion could cascade its
// history away (an arr's deleteFiles=true deletion wipes the grab event
// too, not just the file). That store is what lets a reset still find a
// torrent's tracker after its title is long gone from the arr.
//
// seedarr never deletes anything, torrents or files. Actual removal of a
// stalled-up torrent remains decluttarr's job; seedarr only ever changes
// how long qBittorrent is willing to seed it.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "time/tzdata" // embed tzdata so TZ resolves without OS packages
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	cfg.logFields(logger.Info()).Msg("starting seedarr, configuration above")

	qbt, err := newQbittorrentClient(cfg.QbittorrentURL, cfg.QbittorrentUsername, cfg.QbittorrentPassword, withComponent(logger, "qbittorrent"))
	if err != nil {
		logger.Error().Msg(fmt.Sprintf("could not build qbittorrent client: %v", err))
		os.Exit(1)
	}

	store, err := openGrabStore(cfg.StorePath)
	if err != nil {
		logger.Error().Msg(fmt.Sprintf("could not open grab store at %q: %v", cfg.StorePath, err))
		os.Exit(1)
	}
	defer store.close()

	arr := &arrClient{
		radarrURL:    cfg.RadarrURL,
		radarrAPIKey: cfg.RadarrAPIKey,
		sonarrURL:    cfg.SonarrURL,
		sonarrAPIKey: cfg.SonarrAPIKey,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		log:          withComponent(logger, "arr"),
	}

	reset := &resetter{
		qbittorrent: qbt,
		arr:         arr,
		store:       store,
		log:         withComponent(logger, "reset"),
	}

	trigger := &triggerServer{
		resetter: reset,
		addr:     cfg.TriggerAddr,
		log:      withComponent(logger, "trigger"),
	}

	grabEvents := &grabEventsJob{
		arr:      arr,
		store:    store,
		schedule: cfg.GrabEventsPollSchedule,
		log:      withComponent(logger, "grab-events"),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); grabEvents.run(ctx) }()
	go func() { defer wg.Done(); trigger.run(ctx) }()
	wg.Wait()

	logger.Info().Msg("shutting down")
}
