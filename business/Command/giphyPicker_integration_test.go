//go:build integration

package business

// The composer's GIF picker (GET /command/giphy) against Postgres and Redis.
// It asks Giphy only while the workspace's Giphy app is on and holds a key; a
// page it has fetched is read from the cache the next time, whatever the
// term's case; and the app's switch turns the picker off with /giphy.
// Run: PKGS=./business/Command/ integ_rel.sh <worktree> <log> TestGiphyPicker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestGiphyPickerAsksGiphyOnlyWhileItsAppIsOnWithAKey(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	asked := serveGiphy(t, http.StatusOK, giphyTwoOfMany)

	admin := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'ada', 'Ada', NOW(), NOW())`, admin, admin.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	notOffered := func(when string) {
		t.Helper()
		page, err := GiphyPicker(ctx, "Party", 24)
		if err != nil || page.Configured || len(page.Items) != 0 {
			t.Fatalf("%s: %+v, %v; want not configured, with no GIFs", when, page, err)
		}
	}

	notOffered("no Giphy app")

	app, err := InstallTemplate(ctx, giphyAppSlug, admin)
	if err != nil {
		t.Fatal(err)
	}
	appID, err := uuid.Parse(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	notOffered("installed with no key")

	if _, err := UpdateApp(ctx, appID, commandAdapter.UpdateAppRequest{Secrets: map[string]string{"api_key": "key-1"}}); err != nil {
		t.Fatal(err)
	}
	// The same page three times, the term in another case the third: Giphy is
	// asked once, with the workspace's key, and the rest come from the cache.
	for _, term := range []string{"Party", "Party", "PARTY"} {
		page, err := GiphyPicker(ctx, term, 24)
		if err != nil || !page.Configured || len(page.Items) != 2 || page.Items[0].ID != "g1" {
			t.Fatalf("%q: %+v, %v; want configured, with the two GIFs Giphy can draw", term, page, err)
		}
		if page.NextOffset == nil || *page.NextOffset != 27 {
			t.Fatalf("%q: next page at %s; want 27, after all three GIFs Giphy sent, the skipped one too", term, offsetText(page.NextOffset))
		}
	}
	if n := len(asked); n != 1 {
		t.Fatalf("Giphy asked %d times for one page; want once, and the cache after", n)
	}
	if u := <-asked; u.Query().Get("api_key") != "key-1" || u.Query().Get("q") != "Party" || u.Query().Get("offset") != "24" {
		t.Fatalf("asked %s; want the workspace's key, the term and the offset", u.RawQuery)
	}

	// Giphy out of reach: the error, which is logged, names neither the key
	// nor the address that holds it.
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	reachable := giphyAPI
	giphyAPI = gone.URL + "/v1/gifs"
	_, err = GiphyPicker(ctx, "Party", 48)
	giphyAPI = reachable
	if !errors.Is(err, ErrGiphyUnavailable) || strings.Contains(err.Error(), "key-1") || strings.Contains(err.Error(), "api_key") {
		t.Fatalf("Giphy out of reach: %v; want unavailable, without the key", err)
	}

	// Turned off, the picker goes with /giphy, cached pages and all.
	if err := SetAppEnabled(ctx, appID, false); err != nil {
		t.Fatal(err)
	}
	notOffered("turned off")
	if n := len(asked); n != 0 {
		t.Fatalf("Giphy asked %d more times after the app was turned off; want none", n)
	}
}
