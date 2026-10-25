package basecamp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Basecamp's answers carry the addresses the importer reads next: an
// account's API base, a listing's next page, a project's dock. The token goes
// only to Basecamp's own API hosts, whatever an answer says.
func TestBasecampsTokenStaysOnItsAPIHosts(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			elsewhere.Add(1)
		}
		fmt.Fprint(w, `[]`)
	}))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorization.json":
			fmt.Fprintf(w, `{"accounts":[{"product":"bc3","id":999,"name":"Acme","href":"%s/999"}]}`, other.URL)
		default:
			w.Header().Set("Link", fmt.Sprintf(`<%s/999/people.json?page=2>; rel="next"`, other.URL))
			fmt.Fprint(w, `[]`)
		}
	}))
	defer api.Close()
	allowAPI(t, api.URL)
	prev := launchpadBase
	launchpadBase = api.URL
	t.Cleanup(func() { launchpadBase = prev })

	p := New()
	p.rl = importProvider.NewSleepLimiter(1, 100)
	var people []json.RawMessage
	if err := p.getAll(context.Background(), "tok", api.URL+"/999/people.json", &people); err == nil {
		t.Error("a next page on another host was read")
	}
	items, err := p.accountsOf(context.Background(), "tok")
	if err == nil && len(items) > 0 {
		if _, err := p.projects(context.Background(), "tok", items[0].Meta["api"].(string)); err == nil {
			t.Error("an account whose API base is another host was read")
		}
	}
	if _, err := p.get(context.Background(), "tok", other.URL+"/999/projects.json", &people); err == nil {
		t.Error("a dock address on another host was read")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the token went to another host %d times", n)
	}
}
