package business

import (
	"os"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
)

// protectedBranchWrite must refuse a WRITE tool that targets a default/protected
// branch, generically (driven by readOnlyHint + a branch-like parameter, not a
// hardcoded tool list, and NOT limited to GitHub), while never blocking reads,
// PR creation, branch creation, or feature-branch writes. An explicitly-named
// protected branch is refused on any server (GitLab/Gitea/Bitbucket git MCPs
// too); an empty branch = repo default is refused only for git-hosting servers.
func TestProtectedBranchWrite(t *testing.T) {
	gh := &model.McpServer{Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/"}
	gl := &model.McpServer{Name: "GitLab", URL: "https://gitlab.example.com/mcp"}
	other := &model.McpServer{Name: "Linear", URL: "https://linear-mcp/mcp"}

	cases := []struct {
		name       string
		srv        *model.McpServer
		tool       string
		readOnly   bool
		hasBranch  bool
		params     map[string]string
		wantBlock  bool
		wantBranch string
	}{
		// Writes to the default/protected branch are refused.
		{"push_files to main blocked", gh, "push_files", false, true, map[string]string{"branch": "main"}, true, "main"},
		{"push_files to master blocked", gh, "push_files", false, true, map[string]string{"branch": "master"}, true, "master"},
		{"create_or_update_file to MAIN blocked (case-insensitive)", gh, "create_or_update_file", false, true, map[string]string{"branch": "MAIN"}, true, "MAIN"},
		{"omitted branch = default branch blocked", gh, "create_or_update_file", false, true, map[string]string{}, true, "the default branch"},
		{"delete_file on main blocked", gh, "delete_file", false, true, map[string]string{"branch": "main"}, true, "main"},
		// A future/unknown write tool with a branch param is covered generically.
		{"unknown future write tool covered", gh, "commit_changes", false, true, map[string]string{"branch": "main"}, true, "main"},
		// ref is an accepted branch-like param name.
		{"ref param to master blocked", gh, "push_files", false, true, map[string]string{"ref": "master"}, true, "master"},
		// Non-GitHub git hosts are covered the same way.
		{"gitlab push to main blocked", gl, "push_files", false, true, map[string]string{"branch": "main"}, true, "main"},
		{"gitlab omitted branch = default blocked", gl, "create_or_update_file", false, true, map[string]string{}, true, "the default branch"},
		// Explicit protected name is refused on ANY server (defense in depth).
		{"non-git server explicit main blocked", other, "push_files", false, true, map[string]string{"branch": "main"}, true, "main"},

		// Never blocked.
		{"feature branch write allowed", gh, "push_files", false, true, map[string]string{"branch": "feature/x"}, false, ""},
		{"read tool never blocked", gh, "get_file_contents", true, true, map[string]string{"branch": "main"}, false, ""},
		{"create_pull_request has no branch param", gh, "create_pull_request", false, false, map[string]string{"base": "main", "head": "feat"}, false, ""},
		{"create_branch exempt (branch is the new name)", gh, "create_branch", false, true, map[string]string{"branch": "main"}, false, ""},
		// A non-git server's empty branch is not a repo default → not blocked.
		{"non-git server empty branch not blocked", other, "push_files", false, true, map[string]string{}, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch, blocked := protectedBranchWrite(tc.srv, tc.tool, toolRisk{ReadOnly: tc.readOnly}, tc.hasBranch, tc.params)
			if blocked != tc.wantBlock {
				t.Fatalf("protectedBranchWrite blocked=%v, want %v", blocked, tc.wantBlock)
			}
			if blocked && branch != tc.wantBranch {
				t.Fatalf("branch label = %q, want %q", branch, tc.wantBranch)
			}
		})
	}

	t.Run("configurable protected branches via env", func(t *testing.T) {
		t.Setenv("AI_PROTECTED_BRANCHES", "develop, release")
		if _, blocked := protectedBranchWrite(gh, "push_files", toolRisk{}, true, map[string]string{"branch": "develop"}); !blocked {
			t.Fatal("expected develop to be protected when configured")
		}
		// main is no longer in the custom set, but an empty branch still maps to
		// the repo default and stays protected.
		if _, blocked := protectedBranchWrite(gh, "push_files", toolRisk{}, true, map[string]string{"branch": "main"}); blocked {
			t.Fatal("did not expect main blocked when protected set is {develop,release}")
		}
		if _, blocked := protectedBranchWrite(gh, "push_files", toolRisk{}, true, map[string]string{}); !blocked {
			t.Fatal("empty branch (repo default) must always be protected")
		}
	})

	t.Run("env opt-out disables the guard", func(t *testing.T) {
		t.Setenv("AI_ALLOW_AGENT_DEFAULT_BRANCH_WRITE", "true")
		if _, blocked := protectedBranchWrite(gh, "push_files", toolRisk{}, true, map[string]string{"branch": "main"}); blocked {
			t.Fatal("expected guard disabled when AI_ALLOW_AGENT_DEFAULT_BRANCH_WRITE=true")
		}
		_ = os.Unsetenv("AI_ALLOW_AGENT_DEFAULT_BRANCH_WRITE")
	})
}

// A hostile/buggy server must not be able to disarm the protected-branch guard
// by advertising a mutating tool with readOnlyHint=true. The guard consumes the
// HOST-resolved classification, so the lie is discarded before it reaches here.
func TestProtectedBranchWriteNotBypassedByDishonestReadOnlyHint(t *testing.T) {
	gh := &model.McpServer{Name: "GitHub", URL: "https://api.githubcopilot.com/mcp/"}
	readOnlyLie := &McpToolAnnotations{ReadOnlyHint: true}

	cases := []struct {
		tool   McpTool
		params map[string]string
		branch string
	}{
		// Mutating verb + a read-only lie: still a write, still blocked.
		{McpTool{Name: "push_files", Annotations: readOnlyLie}, map[string]string{"branch": "main"}, "main"},
		{McpTool{Name: "create_or_update_file", Annotations: readOnlyLie}, map[string]string{}, "the default branch"},
		// Destructive name + a read-only lie: still blocked.
		{McpTool{Name: "delete_file", Annotations: readOnlyLie}, map[string]string{"branch": "master"}, "master"},
		// Compound read+mutate name (get_or_create) with the lie: still blocked.
		{McpTool{Name: "get_or_create_file", Annotations: readOnlyLie}, map[string]string{"branch": "main"}, "main"},
		// An unknown-shaped tool with the lie fails closed and stays blocked.
		{McpTool{Name: "do_thing", Annotations: readOnlyLie}, map[string]string{"branch": "main"}, "main"},
	}

	for _, tc := range cases {
		t.Run(tc.tool.Name, func(t *testing.T) {
			risk := classifyToolRisk(tc.tool)
			if risk.ReadOnly {
				t.Fatalf("classifyToolRisk(%q) trusted the readOnlyHint lie", tc.tool.Name)
			}
			branch, blocked := protectedBranchWrite(gh, tc.tool.Name, risk, true, tc.params)
			if !blocked {
				t.Fatalf("protectedBranchWrite(%q) must block a protected-branch write", tc.tool.Name)
			}
			if branch != tc.branch {
				t.Fatalf("branch label = %q, want %q", branch, tc.branch)
			}
		})
	}

	// A genuine read (honest hint + read-shaped name) is still never blocked.
	readRisk := classifyToolRisk(McpTool{Name: "get_file_contents", Annotations: readOnlyLie})
	if !readRisk.ReadOnly {
		t.Fatal("get_file_contents with readOnlyHint should stay read-only")
	}
	if _, blocked := protectedBranchWrite(gh, "get_file_contents", readRisk, true, map[string]string{"branch": "main"}); blocked {
		t.Fatal("a genuine read must never be blocked by the protected-branch guard")
	}
}
