package business

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func withActing(t *testing.T, byUUID map[string]string) {
	t.Helper()
	old := actingEmail
	actingEmail = func(_ context.Context, id string) string { return byUUID[id] }
	t.Cleanup(func() { actingEmail = old })
}

// On the demo, the visitor's assistant could DM or assign work to the demo's
// real members by name. An action taken as one of the demo's own people
// reaches only the demo's own people.
func TestTheDemosAssistantCannotReachARealAccount(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	withActing(t, map[string]string{
		"visitor": "visitor@demo.example",
		"maya":    "maya.chen" + helpers.DemoCastEmailDomain, // the demo's agent acts as Maya
		"agent":   "agent-bot-1" + helpers.BotEmailDomain,
		"owner":   "owner@example.com", // runs the demo, signed in as themselves
	})
	real := &dgraphStruct.DgraphUser{Uuid: "r", UserName: "akash", EmailID: "akash@example.com"}
	cast := &dgraphStruct.DgraphUser{Uuid: "c", UserName: "jonas", EmailID: "jonas.weber" + helpers.DemoCastEmailDomain}
	bot := &dgraphStruct.DgraphUser{Uuid: "b", UserName: "captain", IsBot: true}
	ctx := context.Background()

	for _, actor := range []string{"visitor", "maya", "agent", "unknown"} {
		if err := demoRefuses(ctx, actor, real); err != errDemoPerson {
			t.Errorf("%s reached a real account: %v", actor, err)
		}
		if err := demoRefuses(ctx, actor, cast, bot); err != nil {
			t.Errorf("%s could not reach the demo's own people: %v", actor, err)
		}
		if err := demoRefuses(ctx, actor, cast, real); err != errDemoPerson {
			t.Errorf("%s reached a real account among others: %v", actor, err)
		}
		got := demoReachable(ctx, actor, []*dgraphStruct.DgraphUser{real, cast, nil, bot})
		if len(got) != 2 || got[0] != cast || got[1] != bot {
			t.Errorf("%s found %v by name", actor, got)
		}
	}
	// The people who run the demo use their own assistant as themselves.
	if err := demoRefuses(ctx, "owner", real); err != nil {
		t.Errorf("a demo member's own assistant was refused: %v", err)
	}
	if len(demoReachable(ctx, "owner", []*dgraphStruct.DgraphUser{real, cast})) != 2 {
		t.Error("a demo member's own assistant lost people")
	}
}

func TestEveryOtherServersAssistantReachesEveryone(t *testing.T) {
	t.Setenv("DEMO_MODE", "")
	withActing(t, map[string]string{})
	real := &dgraphStruct.DgraphUser{EmailID: "akash@example.com"}
	if err := demoRefuses(context.Background(), "anyone", real); err != nil {
		t.Errorf("refused off the demo: %v", err)
	}
	if err := demoRefuses(context.Background(), "anyone"); err != nil {
		t.Error(err)
	}
}

// Every place the assistant resolves a person goes through the rule.
func TestEveryAIPersonLookupIsHeldToTheDemoRule(t *testing.T) {
	for _, c := range []struct{ fn, needle string }{
		{"executeSendDM", "demoReachable(ctx, userUUID, foundUsers)"},
		{"executeSendDM", "demoRefuses(ctx, userUUID, sendToDgraphInfo)"},
		{"executeSendDM", "demoRefuses(ctx, userUUID, mentionsDgraphUsersList...)"},
		{"executeSendGroupChat", "demoRefuses(ctx, userUUID, mentionsDgraphUsersList...)"},
		{"resolveAssigneeRef", "demoRefuses(ctx, actingUserUUID, u)"},
		{"resolveAssigneeRef", "demoReachable(ctx, actingUserUUID, found)"},
	} {
		if body := sourceOfFunc(t, "aiExecutors.go", c.fn); !strings.Contains(body, c.needle) {
			t.Errorf("%s does not call %s", c.fn, c.needle)
		}
	}
}

// sourceOfFunc is the source of the top-level func fn in file.
func sourceOfFunc(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
			return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
		}
	}
	t.Fatalf("%s: no func %s", file, fn)
	return ""
}

// On the demo, nothing a demo person starts reaches outside it: not the
// visitor's own agent, not an admin's agent the visitor asked, not a run an
// event started. The people who run the demo, asking their own agents, are
// unaffected, and so is every other server.
func TestTheDemoReachesOutsideOnlyForItsOwnTeam(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	withActing(t, map[string]string{
		"visitor":  "visitor@demo.example",
		"maya":     "maya.chen" + helpers.DemoCastEmailDomain,
		"admin":    "owner@example.com",
		"teammate": "ops@example.com",
	})
	bg := context.Background()
	asked := func(by string) context.Context { return ai.WithRunRequester(bg, by, "admin") }
	refused := []struct {
		why    string
		ctx    context.Context
		acting string
	}{
		{"the visitor's own agent", bg, "visitor"},
		{"the cast's agent", bg, "maya"},
		{"an unknown account", bg, "nobody"},
		{"an admin's agent the visitor DMed", asked("visitor"), "admin"},
		{"an admin's agent someone unidentified asked", asked(""), "admin"},
		{"an admin's agent an event started", auditBusiness.WithInitiator(bg, auditBusiness.InitiatorEvent), "admin"},
		{"an admin's agent another agent handed work", auditBusiness.WithInitiator(bg, auditBusiness.InitiatorHandoff), "admin"},
	}
	for _, c := range refused {
		if err := DemoRefusesOutside(c.ctx, c.acting); err != errDemoOutside {
			t.Errorf("%s reached outside: %v", c.why, err)
		}
		if err := ai.OutsideReachRefused(c.ctx, c.acting); err != errDemoOutside {
			t.Errorf("%s: the registered guard answered %v", c.why, err)
		}
	}
	for _, c := range []struct {
		why    string
		ctx    context.Context
		acting string
	}{
		{"an admin's own run", bg, "admin"},
		{"an admin's schedule", auditBusiness.WithInitiator(bg, auditBusiness.InitiatorSchedule), "admin"},
		{"an admin's agent a teammate asked", asked("teammate"), "admin"},
	} {
		if err := DemoRefusesOutside(c.ctx, c.acting); err != nil {
			t.Errorf("%s was refused: %v", c.why, err)
		}
	}
	if !strings.Contains(errDemoOutside.Error(), "Install OneCamp free") {
		t.Errorf("the refusal must say what to do: %q", errDemoOutside)
	}

	t.Setenv("DEMO_MODE", "")
	if err := DemoRefusesOutside(bg, "visitor"); err != nil {
		t.Errorf("refused off the demo: %v", err)
	}
}
