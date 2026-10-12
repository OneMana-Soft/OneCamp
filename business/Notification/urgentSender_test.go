package notification

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func person() *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserPostgresInfo.Id = uuid.New()
	u.UserPostgresInfo.EmailID = "priya@example.com"
	u.UserDgraphInfo.Uuid = u.UserPostgresInfo.Id.String()
	return u
}

// Notify anyway breaks through someone's pause, and nothing checked who was
// asking. Only a person at the keyboard may; agents, bots and API tokens are
// refused, with words a person can read, before anything is looked up.
func TestOnlyAPersonCanNotifyThroughAPause(t *testing.T) {
	ctx := context.Background()
	if e := senderRefused(ctx, person()); e != nil {
		t.Fatalf("a person was refused: %v", e)
	}

	bot := person()
	bot.UserPostgresInfo.IsBot = true
	agentPrincipal := person()
	agentPrincipal.UserDgraphInfo.IsBot = true
	byAddress := person()
	byAddress.UserPostgresInfo.EmailID = "agent-bot-7" + helpers.BotEmailDomain
	external := person()
	external.UserPostgresInfo.IsExternal = true

	cases := map[string]struct {
		ctx    context.Context
		sender *userModels.UserInfo
	}{
		"no sender":                {ctx, nil},
		"a bot account":            {ctx, bot},
		"an agent's principal":     {ctx, agentPrincipal},
		"a bot by its address":     {ctx, byAddress},
		"an external account":      {ctx, external},
		"an API token (MCP too)":   {context.WithValue(ctx, helpers.ApiTokenIDContextKey, uuid.NewString()), person()},
		"an agent run as a person": {auditBusiness.WithInitiator(ctx, auditBusiness.InitiatorPerson), person()},
		"a handoff from an agent":  {auditBusiness.WithInitiator(ctx, auditBusiness.InitiatorHandoff), person()},
		"a schedule":               {auditBusiness.WithInitiator(ctx, auditBusiness.InitiatorSchedule), person()},
	}
	for name, c := range cases {
		e := senderRefused(c.ctx, c.sender)
		if e == nil || e.Msg != errNotAPerson {
			t.Errorf("%s: got %v, want the not-a-person refusal", name, e)
		}
		// NotifyAnyway itself refuses before it reads anything (no database
		// here), so the check can't be skipped by a caller.
		if c.sender != nil {
			err := NotifyAnyway(c.ctx, c.sender, uuid.New(), time.Now())
			if ue, ok := err.(*UrgentError); !ok || ue.Msg != errNotAPerson {
				t.Errorf("%s: NotifyAnyway answered %v", name, err)
			}
		}
	}
}

// Every way in: the only caller is the session-authenticated controller, and
// no API-token (/v1), MCP or AI tool path calls it.
func TestNotifyAnywayIsReachedOnlyFromASignedInSession(t *testing.T) {
	root := filepath.Join("..", "..")
	var callers []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == "data" || n == ".git" || n == "node_modules" || n == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr == nil && strings.Contains(string(src), "Business.NotifyAnyway(") {
			callers = append(callers, filepath.ToSlash(path))
		}
		return nil
	})
	if len(callers) != 1 || !strings.HasSuffix(callers[0], "controllers/Notification/notificationController.go") {
		t.Errorf("NotifyAnyway is called from %v; only the signed-in controller may call it", callers)
	}
	router, err := os.ReadFile(filepath.Join(root, "router", "router.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(router), "\n")
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "notificationController.NotifyAnyway") {
			n++
			if strings.Contains(l, "v1Router") || strings.Contains(l, "mcp") {
				t.Errorf("notify anyway is served to tokens: %s", strings.TrimSpace(l))
			}
		}
	}
	if n != 1 {
		t.Errorf("notify anyway is routed %d times; want once, on the signed-in user router", n)
	}
}
