package main

import (
	"fmt"
	"os"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

type config struct {
	LogLevel string

	// GrabEventsPollScheduleRaw/GrabEventsPollSchedule govern the
	// background job that captures the arrs' grab events into the store
	// (see jobs.go). It's the only thing seedarr does on a schedule:
	// resets themselves are driven by qui's trigger calls, not polled for.
	// Kept frequent so a grab is captured well before any deletion could
	// cascade its history away.
	GrabEventsPollScheduleRaw string
	GrabEventsPollSchedule    cron.Schedule

	QbittorrentURL      string
	QbittorrentUsername string
	QbittorrentPassword string

	// RadarrURL/RadarrAPIKey and SonarrURL/SonarrAPIKey are how seedarr
	// learns which indexer actually grabbed a torrent (see arr.go), and
	// each indexer's own seed ratio/time requirement — both arrs' indexer
	// entries carry that directly (Prowlarr writes it there when it syncs
	// an indexer in), so there is no Prowlarr URL or API key to configure —
	// seedarr never talks to Prowlarr itself.
	//
	// At least one must be configured, but neither is individually
	// required: a deployment running only movies or only TV is a real
	// configuration, not a misconfiguration. Torrents grabbed by an arr
	// that isn't configured are simply never seedarr's concern, exactly
	// like a manual add.
	RadarrURL    string
	RadarrAPIKey string
	SonarrURL    string
	SonarrAPIKey string

	// StorePath is where seedarr durably remembers hash->indexer mappings
	// (see store.go) — must point at a persistent volume, since its whole
	// purpose is to survive both container restarts and the arrs forgetting
	// a deleted title's own history.
	StorePath string

	// TriggerAddr is where seedarr listens for qui's reset calls (see
	// trigger.go) — the sole way a reset happens. qui calls it as plain
	// curl over the compose network: nothing mounted into qui, no
	// credentials duplicated there, no Docker socket.
	TriggerAddr string
}

// logFields attaches the config as fields on a zerolog event, for the
// startup log line. Credentials are redacted to a presence check — never
// logged in full, even at debug level, since these are live credentials for
// a service that can change how long a torrent seeds.
func (c config) logFields(e *zerolog.Event) *zerolog.Event {
	return e.
		Str("log_level", c.LogLevel).
		Str("qbittorrent_url", c.QbittorrentURL).
		Bool("qbittorrent_credentials_set", c.QbittorrentUsername != "" && c.QbittorrentPassword != "").
		Str("radarr_url", c.RadarrURL).
		Bool("radarr_api_key_set", c.RadarrAPIKey != "").
		Str("sonarr_url", c.SonarrURL).
		Bool("sonarr_api_key_set", c.SonarrAPIKey != "").
		Str("store_path", c.StorePath).
		Str("trigger_addr", c.TriggerAddr).
		Str("grab_events_poll_schedule", c.GrabEventsPollScheduleRaw)
}

// cronParser accepts both standard 5-field cron expressions and the
// "@hourly"/"@every 5m"/etc descriptors — GRAB_EVENTS_POLL_SCHEDULE is
// meant to be set by a human, and the descriptors are the readable common
// case.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func loadConfig() (config, error) {
	cfg := config{
		LogLevel:                  envOr("LOG_LEVEL", "info"),
		GrabEventsPollScheduleRaw: envOr("GRAB_EVENTS_POLL_SCHEDULE", "@every 5m"),
		QbittorrentURL:            envOr("QBITTORRENT_URL", "http://qbittorrent:8080"),
		QbittorrentUsername:       os.Getenv("QBITTORRENT_USERNAME"),
		QbittorrentPassword:       os.Getenv("QBITTORRENT_PASSWORD"),
		RadarrURL:                 envOr("RADARR_URL", "http://radarr:7878"),
		RadarrAPIKey:              os.Getenv("RADARR_API_KEY"),
		SonarrURL:                 envOr("SONARR_URL", "http://sonarr:8989"),
		SonarrAPIKey:              os.Getenv("SONARR_API_KEY"),
		StorePath:                 envOr("STORE_PATH", "/config/seedarr.db"),
		TriggerAddr:               envOr("TRIGGER_ADDR", ":8080"),
	}

	grabEventsSchedule, err := cronParser.Parse(cfg.GrabEventsPollScheduleRaw)
	if err != nil {
		return cfg, fmt.Errorf("invalid GRAB_EVENTS_POLL_SCHEDULE %q: %w", cfg.GrabEventsPollScheduleRaw, err)
	}
	cfg.GrabEventsPollSchedule = grabEventsSchedule

	if cfg.QbittorrentUsername == "" || cfg.QbittorrentPassword == "" {
		return cfg, fmt.Errorf("QBITTORRENT_USERNAME and QBITTORRENT_PASSWORD are required")
	}
	if cfg.RadarrAPIKey == "" && cfg.SonarrAPIKey == "" {
		return cfg, fmt.Errorf("at least one of RADARR_API_KEY or SONARR_API_KEY is required")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
