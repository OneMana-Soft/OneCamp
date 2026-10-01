package codepr

import (
	"strings"
	"testing"
)

func TestSourceID_SurfaceKeys(t *testing.T) {
	repo := RepoRef{Owner: "o", Name: "r"}
	cases := []struct {
		name string
		surf Surface
		want string
	}{
		{"post", Surface{Kind: SurfaceChannelPost, PostID: "p1"}, "post:p1"},
		{"dm", Surface{Kind: SurfaceDM, MessageID: "m1"}, "msg:m1"},
		{"group", Surface{Kind: SurfaceGroupChat, MessageID: "m2"}, "msg:m2"},
		{"task", Surface{Kind: SurfaceTask, TaskID: "t1"}, "task:t1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SourceID(repo, c.surf, "do it"); got != c.want {
				t.Fatalf("SourceID = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSourceID_DedupesSameWork(t *testing.T) {
	repo := RepoRef{Owner: "o", Name: "r"}
	surf := Surface{Kind: SurfaceChannelPost, PostID: "p1"}
	// Two mentions on the SAME post → identical key → coalesce to one job.
	a := SourceID(repo, surf, "fix the padding")
	b := SourceID(repo, surf, "fix the padding please") // different phrasing, same post
	if a != b {
		t.Fatalf("same post must dedupe regardless of phrasing: %q vs %q", a, b)
	}
}

func TestSourceID_FingerprintFallback(t *testing.T) {
	repo := RepoRef{Owner: "o", Name: "r"}
	// No surface id → repo+instruction fingerprint; whitespace-normalized so two
	// identical requests coalesce.
	a := SourceID(repo, Surface{}, "fix   the  bug")
	b := SourceID(repo, Surface{}, "fix the bug")
	if a != b || !strings.HasPrefix(a, "fp:o/r:") {
		t.Fatalf("fingerprint fallback should normalize + dedupe: %q vs %q", a, b)
	}
	// Different repo → different key.
	if SourceID(RepoRef{Owner: "o", Name: "other"}, Surface{}, "fix the bug") == a {
		t.Fatal("different repos must not collide")
	}
}

func TestBuildBudgetInput(t *testing.T) {
	bi := BuildBudgetInput(60, 5, 30, 2, 0, 0, 0, 0, 600, 100, 100, 10)
	if bi.AgentCap.Minutes != 60 || bi.AgentUsage.Runs != 2 || bi.WorkspaceCap.Runs != 100 {
		t.Fatalf("assembly wrong: %+v", bi)
	}
	// Under all caps → allowed.
	if d := CheckBudget(bi); !d.Allowed {
		t.Fatalf("under-cap input should allow, got %+v", d)
	}
	// Agent at run cap → blocks on the agent tier.
	bi2 := BuildBudgetInput(0, 5, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0)
	if d := CheckBudget(bi2); d.Allowed || d.StopReason != StopReasonCodingBudgetAgent {
		t.Fatalf("agent at cap should block, got %+v", d)
	}
}

func TestFormatToolResult(t *testing.T) {
	if got := FormatToolResult(Outcome{Status: StatusOK, Message: "Opened a pull request: url"}); !strings.Contains(got, "url") {
		t.Fatalf("ok should carry the message: %q", got)
	}
	if got := FormatToolResult(Outcome{Status: StatusOK, PRURL: "u"}); !strings.Contains(got, "u") {
		t.Fatalf("ok without message should fall back to PR url: %q", got)
	}
	if got := FormatToolResult(Outcome{Status: StatusBlocked, NeedsHumanQuestion: "Which repo?"}); got != "Which repo?" {
		t.Fatalf("blocked should surface the question: %q", got)
	}
	if got := FormatToolResult(Outcome{Status: StatusNoGreen, Message: "couldn't verify"}); got != "couldn't verify" {
		t.Fatalf("failure should carry the honest message: %q", got)
	}
	if got := FormatToolResult(Outcome{Status: StatusError}); got == "" {
		t.Fatal("must never be empty")
	}
}

func TestAuditFields_NoSecrets(t *testing.T) {
	got := AuditFields(Outcome{Status: StatusOK, Repo: RepoRef{Owner: "o", Name: "r"},
		PRURL: "https://github.com/o/r/pull/1", DiffStat: DiffStat{Files: 2},
		Verifier: VerifierReport{AllPassed: true}, Verdict: Verdict{InScope: true}})
	for _, must := range []string{"status=ok", "repo=o/r", "files=2", "verified=true", "in_scope=true"} {
		if !strings.Contains(got, must) {
			t.Fatalf("audit line missing %q: %s", must, got)
		}
	}
}

func TestAwaitingInputNeedsAQuestion(t *testing.T) {
	if !IsAwaitingInput(Outcome{Status: StatusBlocked, NeedsHumanQuestion: "q"}) {
		t.Fatal("blocked+question is awaiting input")
	}
	if IsAwaitingInput(Outcome{Status: StatusBlocked}) {
		t.Fatal("blocked without a question is not awaiting input")
	}
}
