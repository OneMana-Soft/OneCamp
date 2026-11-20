//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	marketplaceController "github.com/akashc777/OneCamp/controllers/Marketplace"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// crmTemplate is OneCamp's CRM table template (business/Marketplace).
const crmTemplate = "b5d0c000-0000-4000-8000-000000000101"

type aroundAnswer struct {
	Kind        string `json:"kind"`
	EntityID    string `json:"entity_id"`
	Name        string `json:"name"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

// aroundWorld is seedWorkWorld (dee the Web team's admin, cy only in it,
// "Who can make projects" its admins) with the stores a project, a channel
// and a message are written to.
func aroundWorld(t *testing.T) *workWorld {
	t.Helper()
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprint(rw, `{"_index":"x","_id":"x","result":"created","took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })
	return w
}

func installAround(t *testing.T, w *workWorld, who string, body any) (int, aroundAnswer) {
	t.Helper()
	var got aroundAnswer
	code := pastCall(t, marketplaceController.InstallTemplate, http.MethodPost, "/marketplace/templates/"+crmTemplate+"/install",
		body, w.account(who), map[string]string{"id": crmTemplate}, &got)
	return code, got
}

func aroundCount(t *testing.T, w *workWorld, q string, args ...any) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// A table template asked for its project and channel makes them beside its
// tables: both named by the template (numbered on the second install), the
// project in the team asked, its home linking each table, and the channel's
// first message linking the tables and the project.
func TestTableTemplateMakesItsProjectAndChannel(t *testing.T) {
	w := aroundWorld(t)
	dee := w.people["dee"]

	code, got := installAround(t, w, "dee", map[string]string{"project_team_uuid": w.team})
	if code != http.StatusOK || got.EntityID == "" {
		t.Fatalf("dee installs the CRM with its project: %d %+v", code, got)
	}
	if got.ProjectID == "" || got.ProjectName != "CRM" || got.ChannelID == "" || got.ChannelName != "CRM" {
		t.Fatalf("no project and channel around the tables: %+v", got)
	}
	if n := aroundCount(t, w, `SELECT count(*) FROM projects WHERE id = $1 AND team_id = $2 AND project_name = 'CRM' AND deleted_at IS NULL`, got.ProjectID, w.team); n != 1 {
		t.Errorf("the project in the Web team: %d", n)
	}
	if n := aroundCount(t, w, `SELECT count(*) FROM channels WHERE id = $1 AND ch_name = 'CRM' AND created_by = $2`, got.ChannelID, dee); n != 1 {
		t.Errorf("the channel: %d", n)
	}

	// The project's home links each of the tables dee was given, by name.
	rows, err := w.env.PG.Query(`SELECT t.id, t.name, l.url, l.title FROM data_tables t
		LEFT JOIN project_links l ON l.project_uuid = $1 AND l.url LIKE '%/app/tables/' || t.id::text
		WHERE t.created_by = $2 AND t.deleted_at IS NULL`, got.ProjectID, dee)
	if err != nil {
		t.Fatal(err)
	}
	tables := 0
	for rows.Next() {
		var id, name string
		var url, title *string
		if err := rows.Scan(&id, &name, &url, &title); err != nil {
			t.Fatal(err)
		}
		tables++
		if url == nil || title == nil || *title != name {
			t.Errorf("the table %s isn't linked on the project: %v %v", name, url, title)
		}
	}
	rows.Close()
	if tables != 3 || aroundCount(t, w, `SELECT count(*) FROM project_links WHERE project_uuid = $1`, got.ProjectID) != 3 {
		t.Errorf("tables %d, links %d", tables, aroundCount(t, w, `SELECT count(*) FROM project_links WHERE project_uuid = $1`, got.ProjectID))
	}

	// The channel's one message links the first table and the project.
	if n := aroundCount(t, w, `SELECT count(*) FROM posts WHERE post_channel = $1 AND created_by = $2`, got.ChannelID, dee); n != 1 {
		t.Errorf("the channel's messages: %d", n)
	}
	var posts struct {
		Q []struct {
			Posts []struct {
				Text string `json:"post_text"`
			} `json:"ch_posts"`
		} `json:"q"`
	}
	raw := w.dg.Query(t, `{ q(func: eq(ch_uuid, "`+got.ChannelID+`")) { ch_posts { post_text } } }`)
	if err := json.Unmarshal(raw, &posts); err != nil || len(posts.Q) != 1 || len(posts.Q[0].Posts) != 1 {
		t.Fatalf("the channel's message in the graph: %s %v", raw, err)
	}
	text := posts.Q[0].Posts[0].Text
	if !strings.Contains(text, "/app/tables/"+got.EntityID) || !strings.Contains(text, "/app/project/"+got.ProjectID) {
		t.Errorf("the first message doesn't link the table and the project: %s", text)
	}

	// Installed again, both are numbered past the first.
	code, again := installAround(t, w, "dee", map[string]string{"project_team_uuid": w.team})
	if code != http.StatusOK || again.ProjectName != "CRM 2" || again.ChannelName != "CRM 2" {
		t.Errorf("the second install: %d %+v", code, again)
	}

	// Without the option, the install is the one it always was.
	code, plain := installAround(t, w, "dee", nil)
	if code != http.StatusOK || plain.EntityID == "" || plain.ProjectID != "" || plain.ChannelID != "" {
		t.Errorf("a plain install: %d %+v", code, plain)
	}
}

// Someone who may not make projects in the team is refused before anything
// is made: no table, no project, no channel.
func TestTableTemplateAroundRefusedMakesNothing(t *testing.T) {
	w := aroundWorld(t)
	cy := w.people["cy"]
	code, got := installAround(t, w, "cy", map[string]string{"project_team_uuid": w.team})
	if code != http.StatusForbidden {
		t.Fatalf("cy, in the team but not its admin: %d %+v", code, got)
	}
	for what, q := range map[string]string{
		"tables":   `SELECT count(*) FROM data_tables WHERE created_by = $1`,
		"projects": `SELECT count(*) FROM projects WHERE created_by = $1`,
		"channels": `SELECT count(*) FROM channels WHERE created_by = $1`,
	} {
		if n := aroundCount(t, w, q, cy); n != 0 {
			t.Errorf("a refused install made %d %s", n, what)
		}
	}
}
