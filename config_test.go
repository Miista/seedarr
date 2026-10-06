package main

import "testing"

func TestEnvOr_SetAndUnset(t *testing.T) {
	t.Setenv("SEEDARR_TEST_VAR", "custom")
	if got := envOr("SEEDARR_TEST_VAR", "fallback"); got != "custom" {
		t.Fatalf("envOr with the var set = %q, want %q", got, "custom")
	}

	t.Setenv("SEEDARR_TEST_VAR_UNSET", "")
	if got := envOr("SEEDARR_TEST_VAR_UNSET", "fallback"); got != "fallback" {
		t.Fatalf("envOr with the var unset = %q, want %q", got, "fallback")
	}
}

// setRequiredEnv sets every env var loadConfig requires to succeed, so a
// test can override just the one it cares about.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("QBITTORRENT_USERNAME", "user")
	t.Setenv("QBITTORRENT_PASSWORD", "pass")
	t.Setenv("RADARR_API_KEY", "key")
	t.Setenv("SONARR_API_KEY", "sonarr-key")
	// Explicitly cleared so a real value left in the test process's
	// environment (unlikely, but possible) can't leak into a test that
	// expects it unset.
	t.Setenv("GRAB_EVENTS_POLL_SCHEDULE", "")
	t.Setenv("RADARR_URL", "")
	t.Setenv("SONARR_URL", "")
	t.Setenv("TRIGGER_ADDR", "")
}

func TestLoadConfig_AllRequiredVarsSet_Succeeds(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.QbittorrentUsername != "user" || cfg.QbittorrentPassword != "pass" || cfg.RadarrAPIKey != "key" {
		t.Fatalf("loadConfig did not carry through the required env vars: %+v", cfg)
	}
	if cfg.SonarrAPIKey != "sonarr-key" {
		t.Fatalf("expected SONARR_API_KEY to carry through, got %q", cfg.SonarrAPIKey)
	}
	if cfg.SonarrURL != "http://sonarr:8989" {
		t.Fatalf("expected the default SONARR_URL, got %q", cfg.SonarrURL)
	}
	// Defaults apply when the optional vars are empty.
	if cfg.GrabEventsPollScheduleRaw != "@every 5m" {
		t.Fatalf("expected the default GRAB_EVENTS_POLL_SCHEDULE, got %q", cfg.GrabEventsPollScheduleRaw)
	}
	if cfg.TriggerAddr != ":8080" {
		t.Fatalf("expected the default TRIGGER_ADDR, got %q", cfg.TriggerAddr)
	}
}

func TestLoadConfig_MissingQbittorrentCredentials_Fails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("QBITTORRENT_PASSWORD", "")

	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected loadConfig to fail when QBITTORRENT_PASSWORD is unset")
	}
}

func TestLoadConfig_OnlyOneArrConfigured_Succeeds(t *testing.T) {
	// Running only movies, or only TV, is a legitimate configuration — not
	// a misconfiguration. Either arr alone is enough.
	cases := []struct {
		name     string
		clearKey string
	}{
		{name: "radarr only (no sonarr)", clearKey: "SONARR_API_KEY"},
		{name: "sonarr only (no radarr)", clearKey: "RADARR_API_KEY"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(c.clearKey, "")

			if _, err := loadConfig(); err != nil {
				t.Fatalf("expected loadConfig to succeed with only one arr configured: %v", err)
			}
		})
	}
}

func TestLoadConfig_NeitherArrConfigured_Fails(t *testing.T) {
	// With neither arr there is no way to resolve any torrent's indexer at
	// all, so seedarr would have nothing to act on.
	setRequiredEnv(t)
	t.Setenv("RADARR_API_KEY", "")
	t.Setenv("SONARR_API_KEY", "")

	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected loadConfig to fail when neither RADARR_API_KEY nor SONARR_API_KEY is set")
	}
}

func TestLoadConfig_InvalidGrabEventsSchedule_Fails(t *testing.T) {
	// The one schedule seedarr still has must be validated up front — a
	// bad expression should refuse to start, not silently never capture.
	setRequiredEnv(t)
	t.Setenv("GRAB_EVENTS_POLL_SCHEDULE", "not a valid cron expression")

	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected loadConfig to fail for an invalid GRAB_EVENTS_POLL_SCHEDULE")
	}
}
