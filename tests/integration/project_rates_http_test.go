//go:build integration

package integration_test

// S1 through the handler (the 30-day audit): a rates save carries the version
// it read. One made from rates that changed since, or from no read while
// rates exist (a read that failed), answers 409 in words and changes nothing;
// one from the version read is saved and answers the next. A member who
// isn't the project's admin is refused first. Before, a save from a failed
// read replaced the currency and every person's own rate.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestARatesSaveFromAStaleReadIsRefusedThroughTheHandler(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	const live = "0001-01-01T00:00:00Z"
	ada, bob, project := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{ada, "ada"}, {bob, "bob"}} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, u.id, u.name+"@example.test", u.name, u.name); err != nil {
			t.Fatal(err)
		}
	}
	// Ada is the project's admin, Bob a member.
	base := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob.String(), "user_name": "bob", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Billed", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ada"}, {"uid": "_:bob"}}, "project_admins": []map[string]any{{"uid": "_:ada"}}},
	})
	as := func(id uuid.UUID, name string) *userModels.UserInfo {
		return &userModels.UserInfo{
			UserPostgresInfo: userModels.User{Id: id, EmailID: name + "@example.test"},
			UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: base[name], Uuid: id.String(), UserName: name},
		}
	}
	asAda, asBob := as(ada, "ada"), as(bob, "bob")
	params := map[string]string{"project_uuid": project.String()}
	path := "/project/" + project.String() + "/rates"
	type rates struct {
		Data struct {
			Set              bool   `json:"set"`
			Currency         string `json:"currency"`
			DefaultRateCents int64  `json:"default_rate_cents"`
			UpdatedAt        string `json:"updated_at"`
		} `json:"data"`
	}
	answer := func(code int, body []byte) rates {
		t.Helper()
		var r rates
		if code != http.StatusOK {
			t.Fatalf("answered %d: %s", code, body)
		}
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	read := func() rates {
		t.Helper()
		rec := call(t, timeEntryController.ProjectRates, http.MethodGet, path, asAda, "", params)
		return answer(rec.Code, rec.Body.Bytes())
	}
	save := func(who *userModels.UserInfo, currency string, cents int64, version string) (int, string, []byte) {
		rec := call(t, timeEntryController.SetProjectRates, http.MethodPost, path, who,
			fmt.Sprintf(`{"currency":%q,"default_rate_cents":%d,"people":[],"updated_at":%q}`, currency, cents, version), params)
		return rec.Code, msgOf(rec), rec.Body.Bytes()
	}

	if code, _, _ := save(asBob, "USD", 100, ""); code != http.StatusForbidden {
		t.Fatalf("a member who isn't the project's admin saved rates: %d", code)
	}
	first := read()
	if first.Data.Set || first.Data.UpdatedAt != "" {
		t.Fatalf("a project starts with no rates: %+v", first)
	}
	code, _, body := save(asAda, "EUR", 8500, first.Data.UpdatedAt)
	saved := answer(code, body)
	if saved.Data.UpdatedAt == "" {
		t.Fatalf("a save answers no version: %s", body)
	}
	// Another tab saves on top, from the version Ada's save answered.
	code, _, body = save(asAda, "EUR", 9000, saved.Data.UpdatedAt)
	theirs := answer(code, body)

	for _, c := range []struct{ name, version string }{
		{"a read that failed (no version)", ""},
		{"a read from before another save", saved.Data.UpdatedAt},
	} {
		code, msg, _ := save(asAda, "USD", 100, c.version)
		if code != http.StatusConflict || msg == "" {
			t.Fatalf("a save from %s answered %d %q, want 409 in words", c.name, code, msg)
		}
	}
	if now := read(); now.Data.Currency != "EUR" || now.Data.DefaultRateCents != 9000 || now.Data.UpdatedAt != theirs.Data.UpdatedAt {
		t.Fatalf("a refused save changed the rates: %+v, want the other tab's", now)
	}
	if code, _, _ := save(asAda, "EUR", 9500, "yesterday"); code != http.StatusBadRequest {
		t.Fatalf("a version that isn't a time answered %d, want 400", code)
	}
}
