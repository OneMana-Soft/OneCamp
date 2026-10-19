package business

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// teamBranch returns the ResourceTeam arm of PrincipalCanReach, comments stripped.
func teamBranch(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("reach.go")
	if err != nil {
		t.Fatalf("read reach.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
	start := strings.Index(src, "case ResourceTeam")
	if start < 0 {
		t.Fatal("PrincipalCanReach has no ResourceTeam branch")
	}
	rest := src[start:]
	if i := strings.Index(rest, "\n\tcase "); i > 0 {
		rest = rest[:i]
	}
	return rest
}

// A DELETED TEAM STILL LISTS ITS ADMINS. Deleting a team writes a timestamp and leaves
// its admin edges in place, so the membership counts answer "yes, an admin" for a team
// nobody can open — and a project created in one is orphaned on arrival.
//
// The lookup did not select the timestamp at all until this work, which is why nothing
// checked it. Both this branch and the in-app create path are fixed, so the assertion
// covers both.
func TestDeletedTeamIsRefusedOnBothCreatePaths(t *testing.T) {
	if !strings.Contains(teamBranch(t), "IsSoftDeleted(") {
		t.Error("the ResourceTeam branch does not check team liveness, so an agent could " +
			"create a project inside a deleted team")
	}

	// The query must actually return the field, or the check above reads a zero value
	// and silently passes everything.
	raw, err := os.ReadFile("../../domain/Team/teamDomain.go")
	if err != nil {
		t.Fatalf("read teamDomain.go: %v", err)
	}
	src := string(raw)
	at := strings.Index(src, "func GetBasicDgraphTeamInfoByUUID")
	if at < 0 {
		t.Fatal("GetBasicDgraphTeamInfoByUUID is gone; this ratchet has gone stale")
	}
	body := src[at:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "team_deleted_at") {
		t.Error("GetBasicDgraphTeamInfoByUUID does not select team_deleted_at, so every " +
			"liveness check on its result reads a zero value and passes")
	}

	// And the in-app create path, which uses the same lookup and had the same hole.
	app, err := os.ReadFile("../../business/AI/projectAgent.go")
	if err != nil {
		t.Fatalf("read projectAgent.go: %v", err)
	}
	if !strings.Contains(string(app), "IsSoftDeleted(") {
		t.Error("executeCreateProject does not check team liveness; the in-app path would " +
			"still create projects in deleted teams while MCP refuses to — the kind of " +
			"divergence this work exists to remove")
	}
}

// Making a project in a team is the workspace's "Who can make projects"
// (workAccess.CanMakeProjects): the team's admins, or everyone in it, and a token
// as one of its members. It is the rule the app's CreateProject and the agent's
// executeCreateProject ask, so MCP asks it too, or the three drift about who may
// create work in a team (MCP once refused every token and asked everyone else to
// be an admin where the team's members may). Seeing a team decides no write.
func TestMakingAProjectInATeamAsksTheOneRule(t *testing.T) {
	branch := teamBranch(t)

	if !strings.Contains(branch, "ref.Access.IsRead()") {
		t.Fatal("the ResourceTeam branch never consults ref.Access, so creating a project " +
			"would be authorised by the rule for merely seeing the team")
	}
	// The rule must be tied to the write branch, not applied to reads as well —
	// otherwise a member the rule leaves out could not even see the team.
	writeAt := strings.Index(branch, "!ref.Access.IsRead()")
	if writeAt < 0 || !strings.Contains(branch[writeAt:], "workAccess.CanMakeProjects(") {
		t.Error("the write branch does not ask workAccess.CanMakeProjects, so MCP decides who " +
			"makes projects in a team its own way")
	}
	for _, path := range []string{"../../business/AI/projectAgent.go", "../../controllers/Project/projectController.go"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		code := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
		if !strings.Contains(code, "workAccess.CanMakeProjects(") {
			t.Errorf("%s no longer asks workAccess.CanMakeProjects: its rule and MCP's would drift", path)
		}
	}
}

// The container rules for every creation must be the ones the app applies.
func TestCreationsAreBoundToTheirContainer(t *testing.T) {
	byTool := map[string]binding{}
	for _, b := range bridgedTools {
		byTool[b.Tool] = b
	}

	for _, tc := range []struct {
		tool  string
		kind  ResourceKind
		idArg string
	}{
		{"create_task", ResourceProject, "project_uuid"},
		{"create_project", ResourceTeam, "team_uuid"},
		{"create_table_row", ResourceTable, "table_uuid"},
	} {
		b, ok := byTool[tc.tool]
		if !ok {
			t.Errorf("%q is not governed", tc.tool)
			continue
		}
		if b.Kind != tc.kind {
			t.Errorf("%q is authorised against %s; it creates into a %s, and that container "+
				"is what must permit it", tc.tool, b.Kind, tc.kind)
		}
		if b.IDArg != tc.idArg {
			t.Errorf("%q resolves its container from %q, so the authorizer would check a "+
				"different container than the call creates into", tc.tool, b.IDArg)
		}
		// A creation is never naturally idempotent: calling it twice makes two things.
		if b.Behaviour.Idempotent {
			t.Errorf("%q is declared idempotent, but creating twice makes two things — it "+
				"would get no deduplication key and a retry would create a duplicate", tc.tool)
		}
		if b.Behaviour.ReadOnly {
			t.Errorf("%q is declared read-only but creates something", tc.tool)
		}
	}
}

// THE RETRY WINDOW MUST STAY SHORT. The two failure directions are not equally bad: too
// short and a very late retry duplicates, which is visible and correctable; too long and
// a genuine second identical action is answered "already applied" and never happens,
// which is silent. Task names and message text repeat naturally, so a long window makes
// the silent failure the likely one.
func TestTheRetryWindowIsMeasuredInMinutesNotHours(t *testing.T) {
	if claimTTL > time.Hour {
		t.Errorf("claimTTL is %s. A retry window has no reason to exceed an hour: a client "+
			"retries a lost response in seconds and a queued job in minutes. Beyond that "+
			"the window stops catching retries and starts swallowing genuine repeat "+
			"actions — silently, which is the worse of the two errors.", claimTTL)
	}
	if claimTTL < time.Minute {
		t.Errorf("claimTTL is %s, which is shorter than a realistic retry with backoff; "+
			"duplicates would get through", claimTTL)
	}
}
