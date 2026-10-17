//go:build integration
// +build integration

package integration_test

// Does the demo's shared visitor learn anyone's address from search?
//
// The unified search found people by user_email with fuzziness, so a guess
// within two edits of a demo member's address found them, and an exact guess
// confirmed whose it was. And it highlighted the address, which the web app
// shows as a person's context line: highlighted, <mark>someone@example.com</mark>
// is no longer exactly an address, so the visitor's answers kept it
// (helpers.ServeHidingEmails). Now no one's address is highlighted, and the
// visitor finds people by name alone; everyone else still finds them by
// address. Against a real OpenSearch, the people index mapped as the server
// maps it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDemoVisitorFindsNoOneByTheirAddress -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDemoVisitorFindsNoOneByTheirAddress(t *testing.T) {
	ctx := context.Background()
	integration.SetupOpenSearch(t)
	client := opensearchInit.OpenSearchClient

	// Every index the unified search reads: people as opensearchInit maps
	// them, the rest with only the dates it sorts and filters by.
	dates := `"created_date": { "type": "date", "format": "epoch_second" },
		"deleted_date": { "type": "date", "format": "epoch_second" }`
	mappings := map[string]string{openSearchStruct.USER_INDEX: `
		"user_id": { "type": "keyword", "index": true },
		"user_name": { "type": "text", "index": true },
		"user_full_name": { "type": "text", "index": true },
		"user_email": { "type": "keyword", "index": true },` + dates}
	for _, index := range []string{openSearchStruct.CHAT_INDEX, openSearchStruct.POST_INDEX, openSearchStruct.COMMENT_INDEX,
		openSearchStruct.ATTACHMENT_INDEX, openSearchStruct.DOC_INDEX, openSearchStruct.BOARD_INDEX, openSearchStruct.TASK_INDEX,
		openSearchStruct.PROJECT_INDEX, openSearchStruct.CHANNEL_INDEX, openSearchStruct.TEAM_INDEX} {
		mappings[index] = dates
	}
	for index, properties := range mappings {
		if _, err := client.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
			Index: index, Body: strings.NewReader(`{"mappings": {"properties": {` + properties + `}}}`),
		}); err != nil {
			t.Fatalf("create %s: %v", index, err)
		}
	}
	index := func(id, name, full, email string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"user_id": id, "user_name": name, "user_full_name": full,
			"user_email": email, "created_date": time.Now().Unix()})
		if _, err := client.Index(ctx, opensearchapi.IndexReq{
			Index: openSearchStruct.USER_INDEX, DocumentID: id, Body: strings.NewReader(string(raw)),
			Params: opensearchapi.IndexParams{Refresh: "true"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One of the demo's seeded cast, whom the visitor is meant to find by name.
	danaEmail := "dana.whitfield" + helpers.DemoCastEmailDomain
	dana := uuid.NewString()
	index(dana, "dana", "Dana Whitfield", danaEmail)
	// A real account on the same server, which the visitor must not find at all
	// (since 11 Oct, not even by name).
	rae := uuid.NewString()
	index(rae, "rae", "Rae Holloway", "rae.holloway@northwind.test")

	const visitorEmail = "visitor@demo.example"
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", visitorEmail)

	// searchFor answers text for whoever is signed in as email: whether the
	// person with id who is among the people found, and that result's highlight.
	var searchFor func(email, text, who string) (bool, map[string][]string)
	search := func(email, text string) (bool, map[string][]string) { return searchFor(email, text, dana) }
	searchFor = func(email, text, who string) (bool, map[string][]string) {
		t.Helper()
		as := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
			UserPostgresInfo: userModels.User{Id: uuid.New(), EmailID: email},
			UserDgraphInfo:   dgraphStruct.DgraphUser{Uuid: uuid.NewString()},
		})
		body, _ := json.Marshal(map[string]string{"global_search_text": text})
		rec := httptest.NewRecorder()
		globalSearchController.GetUnifiedGlobalSearch(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))).WithContext(as))
		if rec.Code != http.StatusOK {
			t.Fatalf("searching %q as %s: %d %s", text, email, rec.Code, rec.Body.String())
		}
		var out struct {
			Data struct {
				Page []struct {
					User *struct {
						Id string `json:"user_id"`
					} `json:"user"`
					Highlight map[string][]string `json:"highlight"`
				} `json:"page"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, hit := range out.Data.Page {
			if hit.User != nil && hit.User.Id == who {
				return true, hit.Highlight
			}
		}
		return false, nil
	}

	// Her address, exactly and a letter off.
	for _, guess := range []string{danaEmail, "dana.whitfeld" + helpers.DemoCastEmailDomain} {
		// A member finds her by it, as before, with the address not highlighted.
		found, highlight := search("owner@northwind.test", guess)
		if !found {
			t.Errorf("a member searching %q no longer finds Dana", guess)
		}
		if lit, ok := highlight["user_email"]; ok {
			t.Errorf("a member searching %q has Dana's address highlighted: %q", guess, lit)
		}
		// The visitor finds no one by it.
		if found, _ := search(visitorEmail, guess); found {
			t.Errorf("the demo visitor searching %q found Dana", guess)
		}
	}
	// By name, the visitor finds her as anyone does.
	if found, _ := search(visitorEmail, "Dana Whitfield"); !found {
		t.Error("the demo visitor no longer finds people by name, so the check above proves nothing")
	}
	// A real account stays out of the visitor's results whatever is typed, and
	// a member still finds them.
	for _, guess := range []string{"Rae Holloway", "rae", "rae.holloway@northwind.test"} {
		if found, _ := searchFor(visitorEmail, guess, rae); found {
			t.Errorf("the demo visitor searching %q found a real account", guess)
		}
	}
	if found, _ := searchFor("owner@northwind.test", "Rae Holloway", rae); !found {
		t.Error("a member no longer finds a real teammate by name")
	}
	// Off the demo, the same account finds people by address.
	t.Setenv("DEMO_MODE", "")
	if found, _ := search(visitorEmail, danaEmail); !found {
		t.Error("off the demo, the visitor's address alone stopped search finding people by theirs")
	}
}
