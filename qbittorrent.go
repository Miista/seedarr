package main

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// qbittorrentClient talks to qBittorrent's WebUI API. Unlike Radarr/Sonarr,
// qBittorrent uses cookie-based session auth rather than a static API key —
// login() must be called once before any other method, and the underlying
// http.Client must carry a cookie jar so the session cookie survives across
// requests.
type qbittorrentClient struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
	log        zerolog.Logger
}

func newQbittorrentClient(baseURL, username, password string, log zerolog.Logger) (*qbittorrentClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &qbittorrentClient{
		baseURL:  baseURL,
		username: username,
		password: password,
		// Without a timeout, a single hung request (a network blip, a VPN
		// reconnect in front of qBittorrent) blocks its caller forever —
		// an earlier version of this client, with no timeout, stalled
		// seedarr for hours with no error logged.
		httpClient: &http.Client{Jar: jar, Timeout: 15 * time.Second},
		log:        log,
	}, nil
}

// login establishes a session cookie. qBittorrent sessions expire after a
// period of inactivity, and resets arrive whenever qui decides — possibly
// hours apart — so this is called at the start of every reset rather than
// once at startup. One login() per request is cheap and never stale.
func (c *qbittorrentClient) login() error {
	form := url.Values{"username": {c.username}, "password": {c.password}}
	resp, err := c.httpClient.PostForm(c.baseURL+"/api/v2/auth/login", form)
	if err != nil {
		return fmt.Errorf("could not reach qbittorrent to log in: %w", err)
	}
	defer resp.Body.Close()

	// Different qBittorrent/WebUI versions have been observed returning
	// either 200 "Ok." or 204 No Content on a successful login (recent
	// versions return 204) — treat any 2xx as success rather than pinning
	// to one exact code.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("qbittorrent login failed: %s", resp.Status)
	}
	return nil
}

// torrent is the subset of qBittorrent's /torrents/info fields seedarr
// needs: identity, and the two share limits it compares against the
// tracker's floor.
type torrent struct {
	Hash             string  `json:"hash"`
	Name             string  `json:"name"`
	RatioLimit       float64 `json:"ratio_limit"`
	SeedingTimeLimit int64   `json:"seeding_time_limit"`
}

// torrentByHash fetches just one torrent — the trigger is told exactly
// which torrent to act on and has no reason to pull the whole list (easily
// hundreds) to find it. ok=false means qBittorrent doesn't have that hash:
// already deleted, or never there.
func (c *qbittorrentClient) torrentByHash(hash string) (t torrent, ok bool, err error) {
	var out []torrent
	if err := c.get("/api/v2/torrents/info?hashes="+url.QueryEscape(hash), &out); err != nil {
		return torrent{}, false, fmt.Errorf("could not look up torrent %s: %w", hash, err)
	}
	if len(out) == 0 {
		return torrent{}, false, nil
	}
	return out[0], true, nil
}

// setShareLimits applies a per-torrent ratio/seeding-time limit.
// ratioLimit/seedingTimeLimit of -1 mean "unlimited" and -2 means "use the
// global default" — qBittorrent's own convention, reused here rather than
// inventing a different sentinel.
func (c *qbittorrentClient) setShareLimits(hash string, ratioLimit float64, seedingTimeLimit int64) error {
	form := url.Values{
		"hashes":           {hash},
		"ratioLimit":       {strconv.FormatFloat(ratioLimit, 'f', -1, 64)},
		"seedingTimeLimit": {strconv.FormatInt(seedingTimeLimit, 10)},
		// Inactive seeding time limit isn't something seedarr has an
		// opinion on — passing -2 ("use global") leaves whatever the user
		// configured in qBittorrent's own settings alone.
		"inactiveSeedingTimeLimit": {"-2"},
		// Required as of qBittorrent 5.2+ — omitting it returns 400
		// "Missing required parameters: shareLimitAction".
		// "default" defers to whatever action the user configured
		// globally (pause/remove/etc.) once the limit is hit — seedarr
		// only ever decides the limit itself, never what happens after.
		"shareLimitAction": {"default"},
	}
	resp, err := c.httpClient.PostForm(c.baseURL+"/api/v2/torrents/setShareLimits", form)
	if err != nil {
		return fmt.Errorf("could not reach qbittorrent to set share limits for %s: %w", hash, err)
	}
	defer resp.Body.Close()

	// Same 2xx-not-just-200 reasoning as login() — this is an action
	// endpoint with no meaningful response body, so the exact success code
	// isn't something to pin to.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("qbittorrent refused setShareLimits for %s: %s", hash, resp.Status)
	}
	return nil
}

func (c *qbittorrentClient) get(path string, out any) error {
	resp, err := c.httpClient.Get(c.baseURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request to %s failed: %s", path, resp.Status)
	}
	return decodeJSON(resp, out)
}
