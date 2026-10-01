package business

// "Is there a newer OneCamp?", asked for an admin.
//
// A self-hosted workspace moves to a new release only when its admin re-runs
// the installer, and nothing in the app said one existed, so fixes shipped and
// then sat unused on every self-hosted server. (A bot DM once showed under the
// reader's own name; the fix was out, and workspaces that never updated kept
// showing it.)
//
// OneCamp does not contact us on its own: that is what self-hosting is sold
// on. So this runs only when an admin asks, and what it sends is a plain GET
// for the current release on each edition, with nothing about this workspace
// in it. The answer is cached briefly so a page refresh does not ask again.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// Status is what the admin screen shows.
type Status struct {
	// Running is this build's release, "" when it was built from source.
	Running string `json:"running"`
	// Edition is Running's line ("v2"), "" when Running is unknown.
	Edition string `json:"edition"`
	// Latest is the newest release on Edition, "" when unknown.
	Latest string `json:"latest"`
	// LatestByLine is the newest release on every edition.
	LatestByLine    map[string]string `json:"latest_by_line"`
	UpdateAvailable bool              `json:"update_available"`
	// Managed: OneCamp Cloud hosts this workspace and keeps it updated.
	Managed bool `json:"managed"`
	// UpdateCommand is what a self-hosted admin runs on the server, with their
	// licence key in place of the placeholder. Empty when managed.
	UpdateCommand string `json:"update_command,omitempty"`
	// Source is the host that was asked.
	Source    string    `json:"source"`
	CheckedAt time.Time `json:"checked_at"`
}

// installCommandTemplate is the install command from the docs and the licence
// email; re-running it offers the newer release and applies it.
const installCommandTemplate = `/bin/bash -c "$(curl -fsSL https://%s/onecamp/download/YOUR-LICENSE-KEY)"`

const (
	defaultReleasesURL = "https://backend.onemana.dev/onecamp/releases"
	checkCacheTTL      = 10 * time.Minute
	checkTimeout       = 8 * time.Second
)

// Seams.
var (
	fetchReleases = fetchPublishedReleases
	now           = time.Now
)

var cache struct {
	sync.Mutex
	at     time.Time
	byLine map[string]string
}

// Check reports this workspace's release against the current ones.
func Check(ctx context.Context) (Status, error) {
	byLine, at, err := latestByLine(ctx)
	if err != nil {
		return Status{}, err
	}
	return buildStatus(helpers.ReleaseVersion, byLine, managed(), releasesHost(), at), nil
}

// latestByLine returns the current release per edition, from the cache when
// it is fresh.
func latestByLine(ctx context.Context) (map[string]string, time.Time, error) {
	cache.Lock()
	defer cache.Unlock()
	if cache.byLine != nil && now().Sub(cache.at) < checkCacheTTL {
		return cache.byLine, cache.at, nil
	}
	byLine, err := fetchReleases(ctx, releasesURL())
	if err != nil {
		return nil, time.Time{}, err
	}
	cache.byLine, cache.at = byLine, now()
	return byLine, cache.at, nil
}

// buildStatus compares the running release with the published ones. Pure.
func buildStatus(running string, byLine map[string]string, isManaged bool, host string, at time.Time) Status {
	s := Status{Running: strings.TrimSpace(running), LatestByLine: byLine, Managed: isManaged, Source: host, CheckedAt: at}
	if !isManaged && host != "" {
		s.UpdateCommand = fmt.Sprintf(installCommandTemplate, host)
	}
	s.Edition = helpers.ReleaseLine(s.Running)
	if s.Edition != "" {
		s.Latest = byLine[s.Edition]
		s.UpdateAvailable = helpers.CompareReleaseTags(s.Latest, s.Running) > 0
	}
	return s
}

func managed() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("ONECAMP_MANAGED")), "true")
}

// releasesHost is the host the release list comes from, which also serves the
// installer.
func releasesHost() string {
	u, err := url.Parse(releasesURL())
	if err != nil {
		return ""
	}
	return u.Host
}

func releasesURL() string {
	if u := strings.TrimSpace(os.Getenv("ONECAMP_RELEASES_URL")); u != "" {
		return u
	}
	return defaultReleasesURL
}

// fetchPublishedReleases GETs the release list and keeps only well-formed
// release tags, so nothing but a version string from the answer is ever shown.
func fetchPublishedReleases(ctx context.Context, from string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", from, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", from, resp.StatusCode)
	}
	return parseReleases(io.LimitReader(resp.Body, 64<<10))
}

// parseReleases reads {"data":{"latest_by_line":{"v2":"v2.33.0"}}}, keeping
// only entries whose tag belongs to the line it is filed under. Pure.
func parseReleases(r io.Reader) (map[string]string, error) {
	var body struct {
		Data struct {
			LatestByLine map[string]string `json:"latest_by_line"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("unreadable release list: %w", err)
	}
	out := map[string]string{}
	for line, tag := range body.Data.LatestByLine {
		if helpers.ReleaseLine(tag) == line {
			out[line] = tag
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the release list was empty")
	}
	return out, nil
}
