package main

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog"
)

// arrClient talks to Radarr and Sonarr, to answer two things: which
// indexer grabbed a given torrent hash (grabbedIndexerIDs), and what seed
// ratio/time that indexer requires (indexers).
//
// Both questions are answered per arr. A grab is identified by the arr
// that made it plus that arr's own local indexer id, and its requirement
// is read from that same arr's indexer list. Nothing is joined across the
// two arrs: an arr-local id means nothing without knowing which arr it
// came from (the same number is a different tracker in each), but once the
// arr is known it's a complete key, and there is no reason to care that
// two local ids might be the same tracker.
//
// Seed criteria are read from each arr's own indexer entries
// (seedCriteria.seedRatio/seedTime on /api/v3/indexer). Prowlarr is what
// writes them there when it syncs an indexer in, and pushes changes live
// — but seedarr only ever reads the arr's copy and never calls Prowlarr.
//
// Prowlarr enters in exactly one place: Sonarr's grab history carries no
// indexer id, so a Sonarr grab is matched to its indexer via the Prowlarr
// id that Prowlarr embeds in both the grab's downloadUrl and the indexer's
// baseUrl (see grabsFrom). That makes Sonarr grabs through an indexer
// added by hand — not via Prowlarr — unresolvable, which is accepted: the
// URL has nothing to match on, and seedarr won't guess.
//
// Deliberately scoped to grab events only, not imports: whether a
// torrent's data is still genuinely in the library is qui's concern (its
// own hardlink automation calls seedarr's trigger once it's gone), not
// something seedarr itself tracks via the import path — that
// path can be renamed, and more importantly the whole history record
// (including the grab event) is cascade-deleted the moment the arr
// deletes the title, so grab events must be persisted
// durably (see store.go) the moment they're seen, on their own schedule,
// rather than re-derived at reset time.
//
// Torrents added by anything else (Shelfarr's book grabs, a manual add)
// have no seed-criteria enforcement under this design — same as an
// unconfigured tracker, not guessed at via a weaker heuristic.
type arrClient struct {
	radarrURL    string
	radarrAPIKey string
	sonarrURL    string
	sonarrAPIKey string
	httpClient   *http.Client
	log          zerolog.Logger
}

func (a *arrClient) hasRadarr() bool { return a.radarrAPIKey != "" }
func (a *arrClient) hasSonarr() bool { return a.sonarrAPIKey != "" }

// instance is one configured arr, so the two are walked by the same code
// rather than duplicated per service — their /api/v3 history and indexer
// shapes are identical in every respect seedarr uses.
type instance struct {
	name   string
	url    string
	apiKey string
}

func (a *arrClient) instances() []instance {
	var out []instance
	if a.hasRadarr() {
		out = append(out, instance{name: "radarr", url: a.radarrURL, apiKey: a.radarrAPIKey})
	}
	if a.hasSonarr() {
		out = append(out, instance{name: "sonarr", url: a.sonarrURL, apiKey: a.sonarrAPIKey})
	}
	return out
}

// prowlarrIDPattern extracts Prowlarr's own indexer id from a URL it
// generated — both the indexer baseUrl Prowlarr syncs into each arr
// ("http://prowlarr:9696/17/") and the per-grab downloadUrl recorded in
// history ("http://prowlarr:9696/17/download?apikey=...") carry it in the
// same position: the first path segment. Matched on that structure rather
// than on a hardcoded host, since the Prowlarr URL differs per deployment.
var prowlarrIDPattern = regexp.MustCompile(`^[a-zA-Z]+://[^/]+/(\d+)(?:/|$)`)

func prowlarrIDFromURL(rawURL string) (int, bool) {
	m := prowlarrIDPattern.FindStringSubmatch(rawURL)
	if m == nil {
		return 0, false
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return id, true
}

// grabbedIndexerIDs fetches every grab event from every configured arr and
// returns them keyed by torrent hash (lowercased), each mapped to the
// indexer that grabbed it: which arr saw it, and that arr's own local id
// for the indexer. That pair is the whole identity — each grab only ever
// needs to resolve against the arr that made it.
//
// Two resolution paths, because the two arrs differ:
//
//  1. history's own data.indexerId — the arr's LOCAL indexer id, used as
//     is. Radarr populates it on every grab.
//  2. history's data.downloadUrl, for Sonarr, which never populates
//     indexerId at all (it's null on every grab record). Prowlarr
//     generated that URL and embedded its own indexer id in it; the same
//     id sits in the baseUrl of the Sonarr indexer Prowlarr synced in. So:
//     read the id out of the URL, find the Sonarr indexer whose baseUrl
//     carries it, take THAT indexer's local id. The Prowlarr id is used
//     only to make this one match — it's never stored or seen again.
//
// A grab that resolves through neither is skipped, not guessed at. That
// includes Sonarr grabs through an indexer added by hand rather than via
// Prowlarr: its downloadUrl has no Prowlarr id and nothing to match on.
//
// A hash re-grabbed via a different indexer takes its most recent value,
// since callers apply this directly as store upserts in hash order — the
// history API itself returns most-recent-first, but the exact tie-break
// doesn't matter here: the last true re-grab is what should win, and a
// second grab of the same release via the same indexer is a no-op either
// way.
//
// The arrs are queried concurrently and their failures are independent: a
// Sonarr that's down no longer stops Radarr's grabs being captured. That
// matters because capture is time-sensitive — history is cascade-deleted
// when a title is removed, so a grab missed now can be missed for good,
// which is the whole reason this store exists. An error is returned only
// if EVERY configured arr failed, since then there's genuinely nothing to
// record; otherwise the failure is the caller's to log (see jobs.go) and
// the arrs that did answer are still returned.
func (a *arrClient) grabbedIndexerIDs() (map[string]grab, error) {
	instances := a.instances()

	type result struct {
		source string
		grabs  map[string]grab
		err    error
	}
	results := make([]result, len(instances))

	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grabs, err := a.grabsFrom(inst)
			results[i] = result{source: inst.name, grabs: grabs, err: err}
		}()
	}
	wg.Wait()

	// Merged in a.instances()' own fixed order rather than as the
	// goroutines happen to finish, so the result never depends on
	// scheduling. Nothing rides on which arr wins a tie today — a hash is
	// a movie or an episode, never both, so the two histories don't
	// overlap — but a result that varies run to run
	// would be a genuinely unpleasant thing to debug if that ever changed.
	byHash := make(map[string]grab)
	var failures []error
	for _, res := range results {
		if res.err != nil {
			failures = append(failures, res.err)
			continue
		}
		for hash, g := range res.grabs {
			byHash[hash] = g
		}
	}

	if len(failures) == len(instances) {
		return nil, errors.Join(failures...)
	}
	for _, err := range failures {
		a.log.Error().Msg(fmt.Sprintf("could not fetch grab history, continuing with the other arr: %v", err))
	}
	return byHash, nil
}

// grabsFrom resolves one arr's whole grab history. Both of its calls hit
// the same instance, so a failure in either means that arr contributed
// nothing this run — which is exactly the granularity grabbedIndexerIDs
// isolates.
func (a *arrClient) grabsFrom(inst instance) (map[string]grab, error) {
	prowlarrToLocal, err := a.prowlarrToLocalIDs(inst)
	if err != nil {
		return nil, err
	}

	records, err := a.history(inst)
	if err != nil {
		return nil, fmt.Errorf("%s: history: %w", inst.name, err)
	}

	byHash := make(map[string]grab)
	for _, r := range records {
		if r.EventType != "grabbed" || r.DownloadID == "" {
			continue
		}
		localID, ok := resolveLocalIndexerID(r, prowlarrToLocal)
		if !ok {
			continue
		}
		byHash[strings.ToLower(r.DownloadID)] = grab{source: inst.name, arrIndexerID: localID}
	}
	return byHash, nil
}

// resolveLocalIndexerID applies the two resolution paths described on
// grabbedIndexerIDs, in order: the arr's own indexerId first, then the
// Prowlarr id in the downloadUrl matched back to a local indexer. Split out
// from the loop so the preference order itself is directly testable.
func resolveLocalIndexerID(r historyRecord, prowlarrToLocal map[int]int) (int, bool) {
	if localID, err := strconv.Atoi(r.Data.IndexerID); err == nil && localID > 0 {
		return localID, true
	}
	prowlarrID, ok := prowlarrIDFromURL(r.Data.DownloadURL)
	if !ok {
		return 0, false
	}
	localID, ok := prowlarrToLocal[prowlarrID]
	return localID, ok
}

type historyRecord struct {
	DownloadID string            `json:"downloadId"`
	EventType  string            `json:"eventType"`
	Data       historyRecordData `json:"data"`
}

// historyRecordData holds both resolution paths' source fields. Both are
// strings on the wire even where they hold numbers, and
// indexerId is absent entirely on Sonarr rather than empty — which decodes
// to "" either way, exactly the "fall through to downloadUrl" case.
type historyRecordData struct {
	IndexerID   string `json:"indexerId"`   // the arr's own local indexer id; always set on Radarr, never on Sonarr
	DownloadURL string `json:"downloadUrl"` // Prowlarr-generated, embeds Prowlarr's own id; always set on both
}

type historyResponse struct {
	Records []historyRecord `json:"records"`
}

// history fetches every grab event (eventType=1) from one arr, paginated —
// a busy instance's history can run to thousands of records, well past any
// single page's default size.
func (a *arrClient) history(inst instance) ([]historyRecord, error) {
	const pageSize = 250
	var all []historyRecord

	for page := 1; ; page++ {
		reqURL := fmt.Sprintf("%s/api/v3/history?page=%d&pageSize=%d&eventType=1", inst.url, page, pageSize)
		var body historyResponse
		if err := a.get(reqURL, inst.apiKey, &body); err != nil {
			return nil, err
		}
		all = append(all, body.Records...)
		if len(body.Records) < pageSize {
			break
		}
	}
	return all, nil
}

// arrIndexer is the subset of an arr's /api/v3/indexer response seedarr
// needs — its own local id and its full field bag, since both the seed
// criteria and the baseUrl (carrying Prowlarr's own id) live there.
type arrIndexer struct {
	ID     int               `json:"id"`
	Fields []arrIndexerField `json:"fields"`
}

type arrIndexerField struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

func (i arrIndexer) field(name string) (any, bool) {
	for _, f := range i.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return nil, false
}

// prowlarrID reads Prowlarr's own indexer id out of this entry's baseUrl —
// used only to match a Sonarr grab's downloadUrl back to a local indexer
// (see grabsFrom). False for an indexer that wasn't synced from Prowlarr
// (added by hand directly in the arr): there's nothing in its URL to match
// on, so Sonarr grabs through it can't be attributed.
func (i arrIndexer) prowlarrID() (int, bool) {
	v, ok := i.field("baseUrl")
	if !ok {
		return 0, false
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	return prowlarrIDFromURL(s)
}

// seedCriteria reads seedCriteria.seedRatio/seedTime out of the indexer's
// field bag — Prowlarr pushes these into each arr's own indexer entries
// live, so the arr's copy is current and no Prowlarr API call is needed
// to read it (Prowlarr itself is still what put it there). An unset
// field decodes as a nil `any` (the arr represents "no requirement" as a
// JSON null), which is exactly the "not configured" case — not zero, which
// would incorrectly mean "seed for zero time/ratio". The two are tracked
// independently because trackers genuinely configure them independently —
// a seedTime requirement with no ratio requirement at all is a real,
// common configuration.
func (i arrIndexer) seedCriteria() prowlarrSeedFields {
	var f prowlarrSeedFields
	if v, ok := i.field("seedCriteria.seedRatio"); ok {
		if ratio, ok := v.(float64); ok {
			f.ratio, f.ratioOK = ratio, true
		}
	}
	if v, ok := i.field("seedCriteria.seedTime"); ok {
		if minutes, ok := v.(float64); ok {
			f.minutes, f.timeOK = int64(minutes), true
		}
	}
	return f
}

// resolvedIndexer is what a caller needs about one indexer: its seed
// criteria.
type resolvedIndexer struct {
	criteria prowlarrSeedFields
}

// prowlarrToLocalIDs maps the Prowlarr id in each of one arr's indexer
// baseUrls back to that indexer's local id — the lookup grabsFrom needs to
// turn a Sonarr grab's downloadUrl into a local indexer. Indexers not
// synced from Prowlarr have no such id and simply aren't in the map.
func (a *arrClient) prowlarrToLocalIDs(inst instance) (map[int]int, error) {
	indexers, err := a.indexerList(inst)
	if err != nil {
		return nil, err
	}
	prowlarrToLocal := make(map[int]int, len(indexers))
	for _, idx := range indexers {
		if prowlarrID, ok := idx.prowlarrID(); ok {
			prowlarrToLocal[prowlarrID] = idx.ID
		}
	}
	return prowlarrToLocal, nil
}

// indexers fetches every configured arr's indexer list, keyed by source
// arr and then by that arr's own local indexer id — the same (source,
// local id) pair the store persists, so a reset looks a grab up directly
// in the arr that made it. There is no cross-arr merging: a Radarr grab
// resolves against Radarr's list and a Sonarr grab against Sonarr's, and
// nothing ever needs to know that two local ids are the same tracker.
//
// Fetched fresh by the caller rather than cached, since a tracker's seed
// requirement can change (Prowlarr pushes updates into the arrs live) and
// a reset should always apply the current value, not a stale one.
//
// The arrs are queried concurrently, but unlike grabbedIndexerIDs any
// failure fails the whole call: this map decides which floor to APPLY, and
// a half-built one would silently hand torrents the global default
// (resolveTrackerFloor's answer for an indexer it can't find) instead of
// their tracker's real requirement. Failing the reset — qui retries it on
// its next pass — is the right response to that; writing wrong limits is
// not.
func (a *arrClient) indexers() (indexersBySource, error) {
	instances := a.instances()
	lists := make([][]arrIndexer, len(instances))
	errs := make([]error, len(instances))

	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lists[i], errs[i] = a.indexerList(inst)
		}()
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	bySource := make(indexersBySource, len(instances))
	for i, indexers := range lists {
		byLocalID := make(map[int]resolvedIndexer, len(indexers))
		for _, idx := range indexers {
			byLocalID[idx.ID] = resolvedIndexer{criteria: idx.seedCriteria()}
		}
		bySource[instances[i].name] = byLocalID
	}
	return bySource, nil
}

// indexersBySource is every configured arr's indexer list, keyed by source
// arr ("radarr"/"sonarr") and then by that arr's own local indexer id —
// the same pair a stored grab carries, so a lookup is one direct step.
type indexersBySource map[string]map[int]resolvedIndexer

// lookup finds the indexer a grab came through. ok=false means the arr no
// longer has an indexer with that id (removed since the grab) — the caller
// falls back to qBittorrent's global default, since there's no requirement
// left to read.
func (m indexersBySource) lookup(g grab) (resolvedIndexer, bool) {
	idx, ok := m[g.source][g.arrIndexerID]
	return idx, ok
}

// count is the total number of indexers across every arr, for logging.
func (m indexersBySource) count() int {
	n := 0
	for _, byLocalID := range m {
		n += len(byLocalID)
	}
	return n
}

func (a *arrClient) indexerList(inst instance) ([]arrIndexer, error) {
	var indexers []arrIndexer
	if err := a.get(inst.url+"/api/v3/indexer", inst.apiKey, &indexers); err != nil {
		return nil, fmt.Errorf("%s: indexer list: %w", inst.name, err)
	}
	return indexers, nil
}

func (a *arrClient) get(url, apiKey string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request to %s failed: %s", url, resp.Status)
	}
	return decodeJSON(resp, out)
}
