package business

import (
	"context"
	"strings"
	"testing"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The checklist is the first thing a buyer sees, so the failures worth pinning
// are the ones that would make it lie: a step that can never be completed, a
// destination that goes nowhere, or the AI step appearing on the edition that has
// no AI and can therefore never finish it.

func TestEveryStepIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, def := range definitions() {
		s := def.Step
		if s.ID == "" {
			t.Error("a step has no id; the client keys on it")
		}
		if seen[s.ID] {
			t.Errorf("duplicate step id %q", s.ID)
		}
		seen[s.ID] = true

		if s.Title == "" || s.Detail == "" {
			t.Errorf("step %q is missing its title or detail", s.ID)
		}
		// A step with nowhere to go is a to-do list item, not onboarding. The
		// whole point is that each row is one click from being done.
		if s.Href == "" {
			t.Errorf("step %q has no destination", s.ID)
		}
		if def.done == nil {
			t.Errorf("step %q has no way to be completed", s.ID)
		}
		if s.Done {
			t.Errorf("step %q ships pre-completed", s.ID)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no steps defined")
	}
}

func TestThisPackageDefinesNoAIStep(t *testing.T) {
	// THE FAILURE THIS PREVENTS. On the AI-free edition the AI packages are not
	// linked. An AI step written HERE would sit on that edition's checklist
	// forever, unfinishable, for the customers whose policy forbids AI. The AI
	// steps are registered by the AI packages through Register, so the way they
	// disappear is that nothing registers them.
	for _, def := range definitions() {
		if def.ID == "ai" || def.ID == "drill" {
			t.Errorf("step %q is defined in the edition-neutral package; it belongs to the AI packages", def.ID)
		}
	}
}

func TestOnlyTheImportStepCanBeSetAside(t *testing.T) {
	// Skippable is how a step that does not apply to every workspace stays on the
	// list for the ones it does. Marking a universal step skippable would let an
	// admin hide "invite your team" and be told the workspace is fully set up
	// while nobody else can reach it.
	for _, def := range definitions() {
		if def.Skippable && def.ID != "import" {
			t.Errorf("step %q is skippable; only a step that genuinely does not apply to "+
				"every workspace should be", def.ID)
		}
	}
	// And it must not describe one provider's migration to everybody. The
	// pipeline takes eight; the first version of this step said "Bring your Slack
	// history over" and linked to the Slack tab, so a team arriving from Jira read
	// a checklist that did not describe their migration.
	imp := findStep(t, "import")
	if strings.Contains(imp.Title, "Slack") {
		t.Errorf("the import step's title names one provider: %q", imp.Title)
	}
	if strings.Contains(imp.Href, "slack") {
		t.Errorf("the import step links to one provider's tab: %q", imp.Href)
	}
	if !strings.Contains(imp.Detail, "Jira") {
		t.Error("the import step's detail does not mention the other providers, so it " +
			"reads as Slack-only anyway")
	}
	if !imp.Skippable {
		t.Error("the import step is not skippable, so a workspace not coming from Slack " +
			"carries it unfinished forever")
	}
}

func TestSettingAStepAsideRefusesAnythingElse(t *testing.T) {
	// The id arrives from a client, so the rule has to hold here rather than in
	// whatever calls it.
	if err := SetStepSkipped("people", true); err == nil {
		t.Error("a step that is not skippable was set aside anyway")
	}
	if err := SetStepSkipped("no-such-step", true); err == nil {
		t.Error("an unknown step id was accepted")
	}
}

func TestWithMembershipIsTheSameEditBothWays(t *testing.T) {
	// Skip and unskip are one operation in two directions; written separately is
	// how the two drift.
	base := []string{"a", "b"}

	added := withMembership(base, "c", true)
	if len(added) != 3 || added[2] != "c" {
		t.Errorf("adding appended wrongly: %v", added)
	}
	if again := withMembership(added, "c", true); len(again) != 3 {
		t.Errorf("adding twice duplicated: %v", again)
	}
	removed := withMembership(added, "b", false)
	if len(removed) != 2 || removed[0] != "a" || removed[1] != "c" {
		t.Errorf("removing did not preserve the rest in order: %v", removed)
	}
	if missing := withMembership(base, "z", false); len(missing) != 2 {
		t.Errorf("removing something absent changed the list: %v", missing)
	}
}

func TestSplitNonEmptyDropsTheBlanks(t *testing.T) {
	// An empty stored value is the common case — nothing skipped — and
	// strings.Split returns a one-element slice of "" for it, which would read
	// as a step whose id is the empty string.
	if got := splitNonEmpty(""); len(got) != 0 {
		t.Errorf("empty storage read as %v", got)
	}
	if got := splitNonEmpty("a,,b, "); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("blanks survived: %v", got)
	}
}

func TestNonAIStepsApplyToEveryEdition(t *testing.T) {
	// A nil include means unconditional. Channels, people and email exist on both
	// editions, so gating any of them would hide setup from half the customers.
	for _, id := range []string{"channel", "import", "people", "email"} {
		if s := findStep(t, id); s.include != nil {
			t.Errorf("step %q is conditional; it applies to both editions", id)
		}
	}
}

func TestStepOrderMatchesWhatSomebodyWouldActuallyDo(t *testing.T) {
	// Order is content here, not decoration: invite people before email is set up
	// and the invitations silently never arrive. The list is read top to bottom.
	var ids []string
	for _, def := range definitions() {
		ids = append(ids, def.Step.ID)
	}
	// Import sits between the channel and the invitations on purpose: a team
	// migrating wants their history in place BEFORE anyone is invited into the
	// workspace, so the people who arrive land in one that already has it.
	// The password comes first: it is about the account everything else is
	// done from, and it is the credential that has sat in an inbox.
	want := []string{"password", "channel", "import", "people", "email"}
	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("position %d is %q, want %q", i, ids[i], want[i])
		}
	}
}

func findStep(t *testing.T, id string) stepDef {
	t.Helper()
	for _, def := range definitions() {
		if def.Step.ID == id {
			return def
		}
	}
	t.Fatalf("no step with id %q", id)
	return stepDef{}
}

// The contributed-step hook is what lets the AI packages put "watch an agent be
// refused" on the list without this package importing them. These pin the two
// properties a registry like that needs: registration works and is idempotent,
// and a registered step is evaluated by Status the same way a built-in one is.

func resetContributed(t *testing.T) {
	t.Helper()
	contributedMu.Lock()
	saved := contributed
	contributed = nil
	contributedMu.Unlock()
	t.Cleanup(func() {
		contributedMu.Lock()
		contributed = saved
		contributedMu.Unlock()
	})
}

func TestAContributedStepJoinsTheList(t *testing.T) {
	resetContributed(t)
	before := len(allDefinitions())

	Register(Step{ID: "extra", Title: "Extra", Detail: "d", Href: "/x"}, 0, nil,
		func(context.Context, userModels.UserInfo) bool { return false })

	defs := allDefinitions()
	if len(defs) != before+1 {
		t.Fatalf("expected %d steps after registering one, got %d", before+1, len(defs))
	}
	// After the built-ins: the built-in order is the order a person does things
	// in, and a contributed step is something extra they do once those exist.
	if defs[len(defs)-1].ID != "extra" {
		t.Errorf("contributed step should come last, list ends with %q", defs[len(defs)-1].ID)
	}
}

func TestReRegisteringReplacesRatherThanDuplicates(t *testing.T) {
	resetContributed(t)
	done := func(context.Context, userModels.UserInfo) bool { return false }

	Register(Step{ID: "extra", Title: "First", Detail: "d", Href: "/x"}, 0, nil, done)
	Register(Step{ID: "extra", Title: "Second", Detail: "d", Href: "/x"}, 0, nil, done)

	var found []stepDef
	for _, d := range allDefinitions() {
		if d.ID == "extra" {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected one 'extra' step, found %d", len(found))
	}
	if found[0].Title != "Second" {
		t.Errorf("re-registration should replace, kept %q", found[0].Title)
	}
}

func TestRegisterIgnoresAStepThatCannotBeCompleted(t *testing.T) {
	resetContributed(t)
	before := len(allDefinitions())

	Register(Step{ID: "", Title: "no id", Detail: "d", Href: "/x"}, 0, nil,
		func(context.Context, userModels.UserInfo) bool { return false })
	Register(Step{ID: "nodone", Title: "no probe", Detail: "d", Href: "/x"}, 0, nil, nil)

	if got := len(allDefinitions()); got != before {
		t.Errorf("malformed registrations changed the list: %d -> %d", before, got)
	}
}

func TestContributedStepsAreOrderedByWeightNotByInitOrder(t *testing.T) {
	resetContributed(t)
	done := func(context.Context, userModels.UserInfo) bool { return false }

	// Registered heavy-first, the way init order happened to run them.
	Register(Step{ID: "second", Title: "b", Detail: "d", Href: "/x"}, 20, nil, done)
	Register(Step{ID: "first", Title: "a", Detail: "d", Href: "/x"}, 10, nil, done)

	var got []string
	for _, d := range allDefinitions() {
		if d.ID == "first" || d.ID == "second" {
			got = append(got, d.ID)
		}
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("expected [first second], got %v", got)
	}
}

// The password step is about one account, so it is on the list only while that
// account still has the password OneCamp Cloud emailed, and a self-hosted admin
// who chose their own is never asked.
func TestThePasswordStepAppearsOnlyWhileThePasswordIsTheEmailedOne(t *testing.T) {
	def := findStep(t, "password")
	if def.applies == nil {
		t.Fatal("the password step would be shown to every admin")
	}
	orig := hasGeneratedPassword
	t.Cleanup(func() { hasGeneratedPassword = orig })
	for _, generated := range []bool{true, false} {
		hasGeneratedPassword = func(context.Context, userModels.UserInfo) bool { return generated }
		if got := findStep(t, "password").applies(context.Background(), userModels.UserInfo{}); got != generated {
			t.Errorf("generated=%v: applies=%v", generated, got)
		}
	}
	if def.done(context.Background(), userModels.UserInfo{}) {
		t.Error("a step that is only shown while undone reports itself done")
	}
}
