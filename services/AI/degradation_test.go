package ai

import (
	"context"
	"strings"
	"testing"
)

// Reporters are package-level, so each test puts the map back as it found it.
func withNoReporters(t *testing.T) {
	t.Helper()
	degradationMu.Lock()
	saved := degradationReporters
	degradationReporters = map[string]degradationReporter{}
	degradationMu.Unlock()
	t.Cleanup(func() {
		degradationMu.Lock()
		degradationReporters = saved
		degradationMu.Unlock()
	})
}

func TestUnreachableCapabilityReachesTheAnswer(t *testing.T) {
	withNoReporters(t)
	RegisterDegradationReporter("test", func() []DegradedCapability { return []DegradedCapability{{Name: "GitHub"}} })

	// The sink is attached exactly where a surface decides it will explain
	// itself, so the degradation has to be there by the time it is taken.
	n := TakeContextNotice(WithContextNoticeSink(context.Background()))

	if !n.Any() {
		t.Fatal("a broken connector produced no notice, which is the bug this replaces")
	}
	if got := n.Message(); !strings.Contains(got, "GitHub") {
		t.Fatalf("message does not name the connector: %q", got)
	}
}

func TestDegradationOutranksTrimming(t *testing.T) {
	// A shortened prompt is something the system worked around. An unreachable
	// connector is something a person can fix, so it is the one to say.
	n := ContextNotice{Trimmed: true, Degraded: []string{"GitHub"}}
	if got := n.Message(); !strings.Contains(got, "GitHub") {
		t.Fatalf("trimming buried the actionable half: %q", got)
	}
}

func TestDegradedNamesReadAsASentence(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"GitHub"}, "GitHub"},
		{[]string{"GitHub", "Jira"}, "GitHub and Jira"},
		{[]string{"GitHub", "Jira", "Linear"}, "GitHub, Jira and Linear"},
		// Past three it counts, because a dozen broken connectors is one problem.
		{[]string{"GitHub", "Jira", "Linear", "Sentry"}, "GitHub, Jira, Linear and 1 other"},
		{[]string{"a", "b", "c", "d", "e"}, "a, b, c and 2 others"},
	}
	for _, c := range cases {
		if got := joinHumanly(c.in); got != c.want {
			t.Fatalf("joinHumanly(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if joinHumanly(nil) != "" {
		t.Fatal("an empty list should produce nothing to say")
	}
}

func TestSameCapabilityIsNotSaidTwice(t *testing.T) {
	withNoReporters(t)
	// Two subsystems can both notice the same connector is down. A person
	// reading the answer should be told once.
	RegisterDegradationReporter("a", func() []DegradedCapability { return []DegradedCapability{{Name: "GitHub"}} })
	RegisterDegradationReporter("b", func() []DegradedCapability { return []DegradedCapability{{Name: "GitHub"}} })

	ctx := WithContextNoticeSink(context.Background())
	if n := TakeContextNotice(ctx); len(n.Degraded) != 1 {
		t.Fatalf("expected one name, got %v", n.Degraded)
	}
}

func TestNoSinkIsSafe(t *testing.T) {
	// Every background path has no sink. Noting a question and taking a notice
	// must both be free there rather than panicking.
	NoteQuestion(context.Background(), "anything")
	if n := TakeContextNotice(context.Background()); n.Any() {
		t.Error("a context with no sink reported something")
	}
}

// A warning on every answer is one nobody reads by the time it means
// something. A connector that is down is mentioned when the question was about
// what it does, and left out when it was not.
func TestABrokenConnectorIsNamedOnlyWhereItWouldHaveMattered(t *testing.T) {
	withNoReporters(t)
	RegisterDegradationReporter("test", func() []DegradedCapability {
		return []DegradedCapability{{Name: "GitHub", Topics: TopicsForCapability("GitHub", []string{"list_pull_requests", "get_issue"})}}
	})

	for _, q := range []string{
		"what PRs are open on the payments repo",
		"is there an issue about the login bug",
		"is GitHub connected",
		"summarise the pull requests from this week",
	} {
		ctx := WithContextNoticeSink(context.Background())
		NoteQuestion(ctx, q)
		if n := TakeContextNotice(ctx); len(n.Degraded) != 1 {
			t.Errorf("%q: the answer was thinner and did not say so", q)
		}
	}

	for _, q := range []string{
		"write a haiku about the sea",
		"what is on my calendar tomorrow",
		"summarise this thread",
	} {
		ctx := WithContextNoticeSink(context.Background())
		NoteQuestion(ctx, q)
		if n := TakeContextNotice(ctx); len(n.Degraded) != 0 {
			t.Errorf("%q: warned about a connector that could not have helped: %v", q, n.Degraded)
		}
	}
}

// A path that never says what was asked keeps every name, because a person who
// is told about a thinner answer can judge it and a person who is not cannot.
func TestWithNoQuestionEverythingIsStillNamed(t *testing.T) {
	withNoReporters(t)
	RegisterDegradationReporter("test", func() []DegradedCapability {
		return []DegradedCapability{{Name: "GitHub", Topics: []string{"github"}}}
	})
	ctx := WithContextNoticeSink(context.Background())
	if n := TakeContextNotice(ctx); len(n.Degraded) != 1 {
		t.Fatal("silence is the one answer this must never give by default")
	}

	// And a connector whose topics could not be worked out is named whatever
	// was asked, for the same reason.
	withNoReporters(t)
	RegisterDegradationReporter("test", func() []DegradedCapability {
		return []DegradedCapability{{Name: "Mystery"}}
	})
	ctx = WithContextNoticeSink(context.Background())
	NoteQuestion(ctx, "write a haiku about the sea")
	if n := TakeContextNotice(ctx); len(n.Degraded) != 1 {
		t.Fatal("an unknown capability must be reported, not assumed irrelevant")
	}
}

// The topics come from three places, and the third is the one that matters:
// nobody types "GitHub" to ask what PRs are open, and the table that routes
// the built-in tools already knows the words they do type.
func TestTopicsComeFromTheNameTheToolsAndTheCuratedWords(t *testing.T) {
	got := TopicsForCapability("GitHub", []string{"list_pull_requests", "get_issue", "create_branch"})
	has := func(w string) bool {
		for _, g := range got {
			if g == w {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"github", "pull", "requests", "issue", "branch"} {
		if !has(want) {
			t.Errorf("topics are missing %q: %v", want, got)
		}
	}
	// From the curated group for the built-in github tools.
	for _, want := range []string{"prs", "pull request", "merge"} {
		if !has(want) {
			t.Errorf("the curated vocabulary was not used, %q missing: %v", want, got)
		}
	}
	// A verb says what a tool does, not what it is about. "get" would make a
	// connector relevant to every sentence ever typed.
	for _, never := range []string{"get", "list", "create"} {
		if has(never) {
			t.Errorf("%q is a generic verb and must not be a topic: %v", never, got)
		}
	}
}

// The rule itself, without a registry.
func TestRelevantDegradationsDeDuplicatesAndKeepsOrder(t *testing.T) {
	caps := []DegradedCapability{
		{Name: "GitHub", Topics: []string{"github"}},
		{Name: "GitHub", Topics: []string{"github"}},
		{Name: "", Topics: []string{"github"}},
		{Name: "Jira", Topics: []string{"jira"}},
	}
	got := relevantDegradations(caps, "is github down")
	if len(got) != 1 || got[0] != "GitHub" {
		t.Fatalf("got %v, want just GitHub", got)
	}
	if got := relevantDegradations(caps, ""); len(got) != 2 {
		t.Fatalf("with no question both must be named, got %v", got)
	}
}
