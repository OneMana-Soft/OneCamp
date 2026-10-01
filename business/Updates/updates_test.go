package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuildStatus(t *testing.T) {
	at := time.Now()
	byLine := map[string]string{"v1": "v1.19.2", "v2": "v2.33.0"}

	s := buildStatus("v2.26.1", byLine, false, "backend.onemana.dev", at)
	if s.Edition != "v2" || s.Latest != "v2.33.0" || !s.UpdateAvailable {
		t.Fatalf("an older v2 must see the newer v2: %+v", s)
	}
	if s.UpdateCommand != `/bin/bash -c "$(curl -fsSL https://backend.onemana.dev/onecamp/download/YOUR-LICENSE-KEY)"` {
		t.Fatalf("self-hosted gets the install command: %q", s.UpdateCommand)
	}
	if s := buildStatus("v2.33.0", byLine, false, "h", at); s.UpdateAvailable {
		t.Fatalf("current is current: %+v", s)
	}
	if s := buildStatus("v1.19.2", byLine, false, "h", at); s.Latest != "v1.19.2" || s.UpdateAvailable {
		t.Fatalf("an edition is compared with its own line, never another: %+v", s)
	}
	if s := buildStatus("", byLine, false, "h", at); s.Edition != "" || s.UpdateAvailable || s.LatestByLine["v2"] != "v2.33.0" {
		t.Fatalf("a build from source cannot claim an update, but still lists releases: %+v", s)
	}
	if s := buildStatus("v3.0.0", byLine, true, "h", at); s.Latest != "" || s.UpdateAvailable || !s.Managed || s.UpdateCommand != "" {
		t.Fatalf("a line with no published release: %+v", s)
	}
}

func TestParseReleases(t *testing.T) {
	got, err := parseReleases(strings.NewReader(`{"status":"success","data":{"latest_by_line":{"v1":"v1.19.2","v2":"v2.33.0","v3":"v2.1.0","v4":"<script>"}}}`))
	if err != nil || len(got) != 2 || got["v2"] != "v2.33.0" {
		t.Fatalf("only well-formed tags on their own line survive: %v %v", got, err)
	}
	for _, bad := range []string{`not json`, `{"data":{"latest_by_line":{}}}`} {
		if _, err := parseReleases(strings.NewReader(bad)); err == nil {
			t.Errorf("%q must be an error", bad)
		}
	}
}

func TestCheckCachesTheAnswer(t *testing.T) {
	oldFetch, oldNow := fetchReleases, now
	t.Cleanup(func() {
		fetchReleases, now = oldFetch, oldNow
		cache.byLine = nil
	})
	cache.byLine = nil
	clock := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	calls := 0
	fetchReleases = func(context.Context, string) (map[string]string, error) {
		calls++
		return map[string]string{"v2": "v2.33.0"}, nil
	}
	for i := 0; i < 3; i++ {
		if _, err := Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("a fresh answer is reused, asked %d times", calls)
	}
	clock = clock.Add(checkCacheTTL + time.Second)
	_, _ = Check(context.Background())
	if calls != 2 {
		t.Fatalf("a stale answer is refreshed, asked %d times", calls)
	}

	cache.byLine = nil
	fetchReleases = func(context.Context, string) (map[string]string, error) { return nil, errors.New("offline") }
	if _, err := Check(context.Background()); err == nil {
		t.Fatal("an unreachable release list is an error, not 'up to date'")
	}
}
