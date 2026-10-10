//go:build integration
// +build integration

package integration_test

// A member without an @handle reading their profile is given one there
// (business/User.HandleOnRead), against Postgres 12: the profile answers with
// it and it is kept, so the editor never shows an empty handle even before
// the startup backfill reaches them. A read without their name
// (/basicSelfProfile) gives none, and a bot is answered with what it has.
//
// Run: go test -tags=integration ./tests/integration/ -run TestProfileGivesAHandle -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestProfileGivesAHandle(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}

	profileHandle := func(user userModels.UserInfo) string {
		req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, user))
		rec := httptest.NewRecorder()
		userController.GetLoggedInUserProfile(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("profile answered %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				Handle string `json:"user_handle"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Data.Handle
	}
	saved := func(id uuid.UUID) string {
		var h sql.NullString
		if err := env.PG.QueryRow(`SELECT username FROM users WHERE id = $1`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h.String
	}

	member := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya.k@example.test', NOW(), NOW())`, member); err != nil {
		t.Fatal(err)
	}
	// /basicSelfProfile reads Postgres alone (VerifyAuthOnlyPostgres), so
	// the name isn't there: a handle made then would be the address's,
	// @maya.k, and kept for good.
	basic := userModels.UserInfo{UserPostgresInfo: userModels.User{Id: member, EmailID: "maya.k@example.test"}}
	if got := profileHandle(basic); got != "" || saved(member) != "" {
		t.Fatalf("a read without the name gave handle %q, kept %q", got, saved(member))
	}
	user := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: member, EmailID: "maya.k@example.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uuid: member.String(), UserName: "Maya Kapoor"},
	}
	if got := profileHandle(user); got != "maya-kapoor" {
		t.Fatalf("profile handle %q, want maya-kapoor", got)
	}
	if got := saved(member); got != "maya-kapoor" {
		t.Fatalf("kept handle %q, want maya-kapoor", got)
	}
	// Read again: the same handle, not another.
	if got := profileHandle(user); got != "maya-kapoor" {
		t.Fatalf("second read %q", got)
	}
	// Once they have one, a read without the name answers with it.
	if got := profileHandle(basic); got != "maya-kapoor" {
		t.Fatalf("a read without the name answered %q, want maya-kapoor", got)
	}

	bot := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, is_bot, is_external, created_at, updated_at) VALUES ($1, 'bot-y@bots.example.test', true, true, NOW(), NOW())`, bot); err != nil {
		t.Fatal(err)
	}
	botUser := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: bot, EmailID: "bot-y@bots.example.test", IsBot: true, IsExternal: true},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uuid: bot.String(), UserName: "Release Captain"},
	}
	if got := profileHandle(botUser); got != "" || saved(bot) != "" {
		t.Fatalf("a bot was given handle %q", got)
	}
}
