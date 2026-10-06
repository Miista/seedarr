package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// triggerServer is how qui asks seedarr to reset one torrent, the moment
// qui's own hardlink automation decides the library copy is gone. It is
// the only way a reset happens — seedarr has no polling loop, because it
// cannot know on its own which torrents have left the library.
//
// qui calls this through its own external-program feature, as plain curl
// with no wrapper script and nothing mounted:
//
//	/usr/bin/curl -fsS -X POST http://seedarr:8080/reset/{hash}
//
// That's deliberately the least-privileged shape available. The
// alternatives all cost more: mounting seedarr's binary AND its live
// sqlite store into qui (and duplicating the qBittorrent and arr
// credentials into qui's environment), or giving qui the Docker socket so
// it could exec into this container. An HTTP call on the compose network
// needs none of that — seedarr keeps its own config, its own store and its
// own identity.
//
// Retry lives in qui, not here. Its automation matches a torrent on state
// (library copy gone AND ratio still unlimited) and re-evaluates on an
// interval, so a call that fails — seedarr restarting, qBittorrent down —
// leaves the torrent still matching and qui simply calls again next pass.
// A call that succeeds changes the ratio, and the torrent stops matching.
// The status codes below exist to make that work: curl's -f turns a 5xx
// into a visible failure in qui's activity log, and the unchanged state
// turns it into a retry.
type triggerServer struct {
	resetter *resetter
	addr     string
	log      zerolog.Logger
}

// torrentHashPattern is qBittorrent's own v1 infohash shape. The hash
// arrives as a URL path segment from qui's template, so it's validated
// rather than trusted — anything else is rejected before it reaches the
// store or qBittorrent.
var torrentHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func (s *triggerServer) run(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reset/{hash}", s.handleReset)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	s.log.Info().Msg(fmt.Sprintf("listening for reset triggers on %s", s.addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.log.Error().Msg(fmt.Sprintf("trigger server stopped: %v", err))
	}
}

// handleReset resets exactly the torrent qui named.
//
// Status codes are chosen so qui's own activity log stays meaningful with
// curl's -f: 200 for anything seedarr handled correctly (including "not a
// torrent seedarr manages", which is a normal answer, not a failure), 400
// for a malformed hash, 404 for one qBittorrent doesn't have, and 500 only
// for a genuine failure worth seeing — and worth qui retrying.
func (s *triggerServer) handleReset(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(r.PathValue("hash"))
	if !torrentHashPattern.MatchString(hash) {
		s.log.Warn().Msg(fmt.Sprintf("rejected a reset for %q: not a torrent hash", hash))
		http.Error(w, "not a torrent hash\n", http.StatusBadRequest)
		return
	}

	if err := s.resetter.qbittorrent.login(); err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not log in to qbittorrent for %s: %v", hash, err))
		http.Error(w, "qbittorrent unavailable\n", http.StatusInternalServerError)
		return
	}

	t, found, err := s.resetter.qbittorrent.torrentByHash(hash)
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not look up %s: %v", hash, err))
		http.Error(w, "qbittorrent unavailable\n", http.StatusInternalServerError)
		return
	}
	if !found {
		s.log.Warn().Msg(fmt.Sprintf("qbittorrent has no torrent %s", hash))
		http.Error(w, "no such torrent\n", http.StatusNotFound)
		return
	}

	// Fetched per request rather than cached: a tracker's requirement can
	// change in Prowlarr (pushed live into the arrs), and the current value
	// is the one to apply.
	indexers, err := s.resetter.arr.indexers()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not fetch the arrs' indexer lists for '%s': %v", t.Name, err))
		http.Error(w, "arr unavailable\n", http.StatusInternalServerError)
		return
	}

	result, err := s.resetter.applyFloor(t, indexers)
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not reset '%s': %v", t.Name, err))
		http.Error(w, "reset failed\n", http.StatusInternalServerError)
		return
	}

	switch result {
	case outcomeChanged:
		fmt.Fprintf(w, "reset %s\n", t.Name)
	case outcomeAlreadyCorrect:
		fmt.Fprintf(w, "already at its tracker's floor: %s\n", t.Name)
	default:
		// Never grabbed by an arr, so seedarr has no requirement to apply
		// — a normal answer for a book or a manual add, not an error.
		s.log.Debug().Msg(fmt.Sprintf("'%s' was never grabbed by an arr, nothing to reset", t.Name))
		fmt.Fprintf(w, "not grabbed by an arr, nothing to reset: %s\n", t.Name)
	}
}
