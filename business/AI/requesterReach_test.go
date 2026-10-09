package business

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// What the person an agent run acts for can see, resolved for the reads that
// answer with a list. The graph-backed path is proven in tests/integration
// (agent_requester_test.go); these pin the decisions around it.

// withPeople stubs the two lookups viewOf makes.
func withPeople(t *testing.T, person *dgraphStruct.DgraphUser, profile *dgraphStruct.DgraphUser, err error) {
	t.Helper()
	prevPerson, prevProfile := lookupPerson, lookupPersonProfile
	t.Cleanup(func() { lookupPerson, lookupPersonProfile = prevPerson, prevProfile })
	lookupPerson = func(context.Context, string) (*dgraphStruct.DgraphUser, error) { return person, err }
	lookupPersonProfile = func(context.Context, string) (*dgraphStruct.DgraphUser, error) { return profile, err }
}

func TestAPersonsViewIsTheirMemberships(t *testing.T) {
	withPeople(t, &dgraphStruct.DgraphUser{Uid: "0x1"}, &dgraphStruct.DgraphUser{
		Channels: []*dgraphStruct.DgraphChannel{{Uuid: "c1"}, nil, {Uuid: ""}},
		Projects: []*dgraphStruct.DgraphProject{{Uuid: "p1"}},
		Teams:    []*dgraphStruct.DgraphTeam{{Uuid: "t1"}},
		DMs:      []*dgraphStruct.DgraphDm{{GroupingId: "g1"}},
	}, nil)
	v, err := viewOf(context.Background(), "asker")
	if err != nil {
		t.Fatal(err)
	}
	if !v.channels["c1"] || !v.projects["p1"] || !v.teams["t1"] || !v.grpIDs["g1"] || len(v.channels) != 1 {
		t.Errorf("view = %+v", v)
	}
	scope, err := readerScopeFor(context.Background(), "asker")
	if err != nil || strings.Join(scope.Channels, ",") != "c1" || strings.Join(scope.GrpIDs, ",") != "g1" {
		t.Errorf("reader scope = %+v, %v", scope, err)
	}
}

func TestSomeoneWhoMayNotAskSeesNothing(t *testing.T) {
	gone := time.Now()
	cases := map[string]func(t *testing.T){
		"unidentified": func(t *testing.T) {
			withPeople(t, &dgraphStruct.DgraphUser{Uid: "0x1"}, &dgraphStruct.DgraphUser{}, nil)
			if _, err := viewOf(context.Background(), " "); !errors.Is(err, errAskerUnknown) {
				t.Errorf("got %v", err)
			}
		},
		"unresolvable": func(t *testing.T) {
			withPeople(t, nil, nil, errors.New("graph down"))
			if _, err := viewOf(context.Background(), "asker"); err == nil {
				t.Error("a failed lookup must refuse")
			}
		},
		// Deactivation leaves every membership edge in place; the eligibility
		// rule is what stops an offboarded person's view from still answering.
		"deactivated": func(t *testing.T) {
			withPeople(t, &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &gone},
				&dgraphStruct.DgraphUser{Channels: []*dgraphStruct.DgraphChannel{{Uuid: "c1"}}}, nil)
			if _, err := viewOf(context.Background(), "asker"); err == nil {
				t.Error("a deactivated person must not have a view")
			}
		},
		"a bot": func(t *testing.T) {
			withPeople(t, &dgraphStruct.DgraphUser{Uid: "0x1", IsBot: true}, &dgraphStruct.DgraphUser{}, nil)
			if _, err := viewOf(context.Background(), "asker"); err == nil {
				t.Error("a bot is not a person anyone acts for")
			}
		},
	}
	for name, run := range cases {
		t.Run(name, run)
	}
}

func TestARunForTheSponsorIsNotNarrowed(t *testing.T) {
	withPeople(t, nil, nil, errors.New("must not be asked"))
	if v, err := askerView(context.Background()); v != nil || err != nil {
		t.Errorf("a run nobody asked for narrowed to %+v (%v)", v, err)
	}
	ctx := ai.WithRunRequester(context.Background(), "sponsor", "sponsor")
	if v, err := askerView(ctx); v != nil || err != nil {
		t.Errorf("a run the sponsor asked for narrowed to %+v (%v)", v, err)
	}
	if tables, err := askerTables(ctx); tables != nil || err != nil {
		t.Errorf("tables narrowed for the sponsor: %v %v", tables, err)
	}
	if sources, err := askerDataSources(ctx); sources != nil || err != nil {
		t.Errorf("data sources narrowed for the sponsor: %v %v", sources, err)
	}
	unknown := ai.WithRunRequester(context.Background(), "", "sponsor")
	if _, err := askerTables(unknown); !errors.Is(err, errAskerUnknown) {
		t.Errorf("tables for an unidentified asker: %v", err)
	}
	if _, err := askerDataSources(unknown); !errors.Is(err, errAskerUnknown) {
		t.Errorf("data sources for an unidentified asker: %v", err)
	}
}

func TestOnlyTheScopesBothShareAreKept(t *testing.T) {
	got := keepIn([]string{"a", "b", "c"}, map[string]bool{"c": true, "a": true, "z": true})
	if strings.Join(got, ",") != "a,c" {
		t.Errorf("got %v, want the shared ones in the first list's order", got)
	}
	if got := keepIn([]string{"a"}, nil); len(got) != 0 {
		t.Errorf("nothing shared must keep nothing, got %v", got)
	}
}

func TestANameSearchForSomeoneElseKeepsWhatBothFind(t *testing.T) {
	mine := []UnifiedHit{
		{ContentType: "doc", DocUUID: "d1"},
		{ContentType: "doc", DocUUID: "d2"},
		{ContentType: "project", ProjectUUID: "p1"},
		{ContentType: "task", TaskUUID: "t1", ProjectUUID: "p1"},
		{Title: "names nothing"},
	}
	theirs := []UnifiedHit{
		{ContentType: "doc", DocUUID: "d2"},
		{ContentType: "task", TaskUUID: "t1", ProjectUUID: "p9"},
		{ContentType: "project", ProjectUUID: "p2"},
		{Title: "names nothing"},
	}
	var got []string
	for _, h := range sharedHits(mine, theirs) {
		got = append(got, namedHitKey(h))
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "doc:d2 task:t1" {
		t.Errorf("shared = %v, want the doc and the task both found, and nothing that names no object", got)
	}
}

func TestASearchForSomeoneElseLeavesTheSponsorsAccountsAlone(t *testing.T) {
	called := map[string]bool{}
	searcher := func(source string) unifiedSearcher {
		return unifiedSearcher{source: source, label: strings.ToUpper(source), run: func(context.Context) ([]UnifiedHit, bool, string) {
			called[source] = true
			return []UnifiedHit{{Title: source}}, true, ""
		}}
	}
	out := withoutPersonalSources([]unifiedSearcher{
		searcher(UnifiedSourceWorkspace), searcher(UnifiedSourceMemory), searcher(UnifiedSourceGmail), searcher(UnifiedSourceGitHub),
	})
	if len(out) != 4 {
		t.Fatalf("the sources must all still report, got %d", len(out))
	}
	for _, s := range out {
		hits, connected, note := s.run(context.Background())
		switch s.source {
		case UnifiedSourceGmail, UnifiedSourceGitHub:
			if len(hits) != 0 || connected || note == "" {
				t.Errorf("%s was searched for someone else: %v %v %q", s.source, hits, connected, note)
			}
		default:
			if len(hits) != 1 {
				t.Errorf("%s, a workspace source, was dropped", s.source)
			}
		}
	}
	if called[UnifiedSourceGmail] || called[UnifiedSourceGitHub] {
		t.Error("a connected account was queried at all")
	}
}

// A run follows the standing instructions of the people it acts for, and
// nobody else's: its sponsor's, and the asker's when someone else asked.
func TestARunFollowsOnlyItsOwnPeoplesInstructions(t *testing.T) {
	sponsor := "6f1c3b0e-7d2a-4c55-9a43-0b1f2c3d4e5f"
	asker := "0a9b8c7d-6e5f-4a3b-8c2d-1e0f9a8b7c6d"
	cases := []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{"nobody asked", ai.WithoutRunRequester(context.Background()), []string{sponsor}},
		{"the sponsor asked", ai.WithRunRequester(context.Background(), sponsor, sponsor), []string{sponsor}},
		{"someone else asked", ai.WithRunRequester(context.Background(), asker, sponsor), []string{sponsor, asker}},
		{"someone unidentified asked", ai.WithRunRequester(context.Background(), "", sponsor), []string{sponsor}},
		{"for an agent with no sponsor", ai.WithRunRequester(context.Background(), asker, "not-a-person"), []string{asker}},
	}
	for _, c := range cases {
		sponsorOf := sponsor
		if c.name == "for an agent with no sponsor" {
			sponsorOf = "not-a-person"
		}
		if got := instructionAuthors(c.ctx, sponsorOf); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if got := AgentScopedMemoryBlock(context.Background(), "c1", "", "not-a-person"); got != "" {
		t.Errorf("an agent with no sponsor follows instructions: %q", got)
	}
}

// summarize_group_chat reads the conversation its declared parameter names, the
// one a call is authorised against, and no undeclared alias.
func TestSummarizeGroupChatReadsOnlyItsDeclaredConversation(t *testing.T) {
	_, _, err := executeSummarizeGroupChat(context.Background(), ai.ProposedAction{
		ToolName: "summarize_group_chat",
		Params:   map[string]string{"grouping_id": "the-sponsors-dm"},
	}, "sana")
	if err == nil || !strings.Contains(err.Error(), "grp_id is required") {
		t.Errorf("a call naming its conversation only by an undeclared parameter was not refused: %v", err)
	}
}

// A refusal about the person who asked reads as being about them, not "the
// originating person", and never mentions a sponsor's access it didn't use.
func TestRefusalsAboutTheAskerNameThem(t *testing.T) {
	gone := time.Now().Add(-time.Hour)
	withPeople(t, &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "ravi", DeletedAt: &gone}, &dgraphStruct.DgraphUser{}, nil)
	_, err := viewOf(context.Background(), "ravi")
	if err == nil || err.Error() != "the account of the person who asked is deactivated" {
		t.Errorf("a deactivated asker: %v", err)
	}
	if strings.Contains(errAskerUnknown.Error(), "sponsor") {
		t.Errorf("unknown asker: %q", errAskerUnknown.Error())
	}
}

// What a run for someone else resolves about them, it resolves once: a search
// alone asks it for every index query it makes, and each answer is several
// graph reads. A new run asks again, so a change of membership is seen.
func TestARunResolvesItsAskerOnce(t *testing.T) {
	prevPerson, prevProfile := lookupPerson, lookupPersonProfile
	t.Cleanup(func() { lookupPerson, lookupPersonProfile = prevPerson, prevProfile })
	persons, profiles := 0, 0
	lookupPerson = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		persons++
		return &dgraphStruct.DgraphUser{Uid: "0xravi", Uuid: "ravi"}, nil
	}
	lookupPersonProfile = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		profiles++
		return &dgraphStruct.DgraphUser{Channels: []*dgraphStruct.DgraphChannel{{Uuid: "c1"}}}, nil
	}
	run := ai.WithRunRequester(context.Background(), "ravi", "sana")
	for i := 0; i < 3; i++ {
		if v, err := askerView(run); err != nil || !v.channels["c1"] {
			t.Fatalf("asker view: %+v, %v", v, err)
		}
	}
	for i := 0; i < 2; i++ {
		if s, err := readerScopeFor(run, "ravi"); err != nil || len(s.Channels) != 1 {
			t.Fatalf("reader scope: %+v, %v", s, err)
		}
	}
	if persons != 1 || profiles != 1 {
		t.Errorf("one run looked its asker up %d times and read their profile %d times, want once each", persons, profiles)
	}
	if _, err := askerView(ai.WithRunRequester(context.Background(), "ravi", "sana")); err != nil || profiles != 2 {
		t.Errorf("a new run reused the last run's answer (profiles read %d, err %v)", profiles, err)
	}
}

// The code analysis follows the standing instructions of the agent's sponsor,
// taken from the run, not of whoever the call executes as.
func TestCodeContextFollowsTheRunsSponsor(t *testing.T) {
	sponsor, asker := "6f1c3b0e-7d2a-4c55-9a43-0b1f2c3d4e5f", "0a9b8c7d-6e5f-4a3b-8c2d-1e0f9a8b7c6d"
	if got := runSponsor(ai.WithRunRequester(context.Background(), asker, sponsor), "the-job-owner"); got != sponsor {
		t.Errorf("for someone else's run: %q, want the sponsor", got)
	}
	if got := runSponsor(ai.WithoutRunRequester(context.Background()), sponsor); got != sponsor {
		t.Errorf("for the sponsor's own run: %q", got)
	}
}
