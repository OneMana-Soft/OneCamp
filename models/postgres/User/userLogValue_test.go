package models

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// hydratedUserInfo mirrors what the six middlewares actually put in the request context:
// GetActiveDgraphUserInfoByUUID fills in the user's channels, projects, teams and DMs, and the DMs
// carry other people's names and profile keys.
func hydratedUserInfo(t *testing.T) UserInfo {
	t.Helper()

	id := uuid.MustParse("3f2504e0-4f89-11d3-9a0c-0305e82c3301")
	profileKey := "avatars/alice.png"
	username := "alice"
	display := "Alice Anderson"
	ghLogin := "alice-a"
	now := time.Now()

	return UserInfo{
		UserPostgresInfo: User{
			Id:          id,
			EmailID:     "alice.anderson@example.com",
			Username:    &username,
			DisplayName: &display,
			GitHubLogin: &ghLogin,
			IsAdmin:     true,
			IsExternal:  false,
			IsBot:       false,
			CreatedAt:   now,
		},
		UserDgraphInfo: dgraphStruct.DgraphUser{
			Uid:          "0x2a1b",
			Uuid:         id.String(),
			UserName:     "alice",
			UserFullName: "Alice Anderson",
			EmailID:      "alice.anderson@example.com",
			Title:        "Staff Engineer",
			Hobbies:      "climbing",
			ProfileKey:   &profileKey,
			Channels: []*dgraphStruct.DgraphChannel{
				{Uuid: "c-1", Name: "acquisition-project-secret"},
				{Uuid: "c-2", Name: "general"},
			},
			Projects: []*dgraphStruct.DgraphProject{
				{Uuid: "p-1", Name: "Project Redacted"},
			},
			Teams: []*dgraphStruct.DgraphTeam{
				{Uuid: "t-1", Name: "Platform"},
			},
		},
	}
}

// renderLog writes one record through a real JSON handler, the same way the stdout sink does, and
// returns what was emitted. Asserting on the emitted bytes is the point: the defect was invisible
// at the call site and only existed in the output.
func renderLog(t *testing.T, value any) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.LogAttrs(context.Background(), slog.LevelError, "something failed",
		slog.Any("user_info", value))
	return buf.String()
}

// The requirement is kept: a log line still says exactly which user it concerns.
func TestUserInfoLogValueStillIdentifiesTheUser(t *testing.T) {
	out := renderLog(t, hydratedUserInfo(t))

	if !strings.Contains(out, "3f2504e0-4f89-11d3-9a0c-0305e82c3301") {
		t.Errorf("the user's uuid is missing, so the line cannot be attributed:\n%s", out)
	}
	// The flags that change triage.
	for _, want := range []string{`"is_admin":true`, `"is_external":false`, `"is_bot":false`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %s in the record:\n%s", want, out)
		}
	}
}

// Nothing that fails to help answer "which user" should be there — including, most importantly,
// other people.
func TestUserInfoLogValueOmitsPersonalAndUnrelatedData(t *testing.T) {
	out := renderLog(t, hydratedUserInfo(t))

	for _, leaked := range []struct{ what, needle string }{
		{"the user's email", "alice.anderson@example.com"},
		{"their username", `"alice"`},
		{"their display name", "Alice Anderson"},
		{"their GitHub login", "alice-a"},
		{"their job title", "Staff Engineer"},
		{"their hobbies", "climbing"},
		{"their profile key", "avatars/alice.png"},
		{"a channel name", "acquisition-project-secret"},
		{"a project name", "Project Redacted"},
		{"a team name", "Platform"},
	} {
		if strings.Contains(out, leaked.needle) {
			t.Errorf("%s (%q) is still in the log record:\n%s", leaked.what, leaked.needle, out)
		}
	}
}

// The size claim, asserted rather than described. A hydrated UserInfo serialised whole runs to
// kilobytes per line; the summary must stay small enough that logging it on every line is free.
func TestUserInfoLogValueIsSmall(t *testing.T) {
	out := renderLog(t, hydratedUserInfo(t))

	// Generous ceiling: the whole record including message and timestamp. A hydrated UserInfo
	// alone measured ~5.5 KB for a typical user and ~20 KB for a heavy one.
	const ceiling = 400
	if len(out) > ceiling {
		t.Errorf("record is %d bytes, want under %d — the summary is not doing its job:\n%s",
			len(out), ceiling, out)
	}
}

// A zero value must not panic. Log calls happen on error paths, where a half-built UserInfo is
// exactly what might be in the context.
func TestUserInfoLogValueHandlesZeroValue(t *testing.T) {
	out := renderLog(t, UserInfo{})

	if !strings.Contains(out, "00000000-0000-0000-0000-000000000000") {
		t.Errorf("a zero UserInfo should still render its nil uuid rather than panic:\n%s", out)
	}
}

// End to end through the REAL logging path, which is what production uses.
//
// The tests above prove slog resolves LogValuer through a JSON handler. This proves the thing that
// actually matters: that a UserInfo sitting in the request context under
// helpers.UserInfoContextKey — put there by all six middlewares, and picked up automatically
// because it is in helpers.DefaultContextKeys — is summarised rather than dumped whole when a
// handler calls helpers.LogErrorWithContext.
//
// This test lives here rather than in helpers because helpers cannot import this package: it is
// imported BY it. That constraint is also why LogValue is defined on the type instead of the
// logging layer narrowing the value itself.
func TestUserInfoIsSummarisedThroughTheRealLoggingPath(t *testing.T) {
	var buf bytes.Buffer
	prev := helpers.Logger
	helpers.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { helpers.Logger = prev })

	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, hydratedUserInfo(t))
	helpers.LogErrorWithContext(ctx, "controllers/CreateChannel failed err: %+v", context.DeadlineExceeded)

	out := buf.String()

	// Still attributable.
	if !strings.Contains(out, "3f2504e0-4f89-11d3-9a0c-0305e82c3301") {
		t.Errorf("the request's user cannot be identified from the log line:\n%s", out)
	}
	// And no longer carrying the graph or anyone's personal data.
	for _, leaked := range []string{
		"alice.anderson@example.com",
		"Alice Anderson",
		"acquisition-project-secret",
		"avatars/alice.png",
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("%q reached the log through the real path:\n%s", leaked, out)
		}
	}
	// The message itself must still be rendered, not left as a literal verb.
	if !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("the error was not rendered into the message:\n%s", out)
	}
}
