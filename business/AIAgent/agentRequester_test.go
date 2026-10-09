package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcpServer "github.com/akashc777/OneCamp/business/MCPServer"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// What an agent may do for someone other than its sponsor. The decisions that
// need a graph are proven in tests/integration (agent_requester_test.go); these
// are the ones that must hold without one.

func someoneElsesRun(sponsor uuid.UUID) (context.Context, *requesterGate) {
	ctx := ai.WithRunRequester(context.Background(), uuid.NewString(), sponsor.String())
	requester, _, _ := ai.RunRequester(ctx)
	return ctx, &requesterGate{requester: requester, sponsorName: "Sana"}
}

func TestTheSponsorsOwnReachIsNeverLent(t *testing.T) {
	ctx, gate := someoneElsesRun(uuid.New())
	want := map[string]string{
		"gmail_search":          "only Sana can ask me to use their connected accounts",
		"gmail_send":            "only Sana can ask me to use their connected accounts",
		"calendar_list_events":  "only Sana can ask me to use their connected accounts",
		"calendar_create_event": "only Sana can ask me to use their connected accounts",
		"github_list_prs":       "only Sana can ask me to use their connected accounts",
		"github_list_issues":    "only Sana can ask me to use their connected accounts",
		"github_comment":        "only Sana can ask me to use their connected accounts",
		"set_reminder":          "only Sana can ask me to use their calendar",
		"summarize_dm":          "only Sana can ask me to use their direct messages",
		"send_dm":               "only Sana can ask me to use their direct messages",
	}
	for tool, refusal := range want {
		if got := gate.refusal(ctx, ai.ProposedAction{ToolName: tool, Params: map[string]string{"to_uuid": uuid.NewString(), "to_user_uuid": uuid.NewString()}}); got != refusal {
			t.Errorf("%s for someone else: got %q, want %q", tool, got, refusal)
		}
	}
	if len(personalTools) != len(want) {
		t.Errorf("personalTools has %d entries and this test checks %d; a new personal tool needs its refusal pinned here", len(personalTools), len(want))
	}
}

func TestAnAskerWhoCannotBeIdentifiedGetsNothing(t *testing.T) {
	gate := &requesterGate{requester: "", sponsorName: "Sana"}
	for _, tool := range []string{"search_workspace", "read_doc", "list_projects", "web_search"} {
		got := gate.refusal(context.Background(), ai.ProposedAction{ToolName: tool, Params: map[string]string{"doc_uuid": uuid.NewString()}})
		if !strings.Contains(got, "couldn't tell who asked") {
			t.Errorf("%s ran for an unidentified asker: %q", tool, got)
		}
	}
}

func TestARefusalNamesWhatTheAskerLacksInPlainWords(t *testing.T) {
	cases := map[string]string{
		"the originating person is not a member of this channel":    "they're not a member of this channel",
		"the originating person has no grant on this private doc":   "they have no grant on this private doc",
		"the originating person may not edit this doc":              "they may not edit this doc",
		"the originating person's account is deactivated":           "their account is deactivated",
		"the originating person could not be resolved":              "they could not be resolved",
		"changing this task requires being an admin of its project": "changing this task requires being an admin of its project",
	}
	for in, want := range cases {
		if got := askerReason(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestACallWhoseTargetCannotBeReadIsRefused(t *testing.T) {
	ctx, gate := someoneElsesRun(uuid.New())
	// No doc named: the governed reading refuses rather than widening to the
	// workspace, and nothing is looked up as the sponsor.
	got := gate.refusal(ctx, ai.ProposedAction{ToolName: "read_doc", Params: map[string]string{}})
	if !strings.Contains(got, "could not tell what read_doc would act on") {
		t.Errorf("got %q", got)
	}
}

func TestALookupThatCannotBeMadeIsARefusal(t *testing.T) {
	// No graph is connected in a unit test, so the authority lookup cannot run:
	// that must read as "no", never as "the sponsor may, so go ahead".
	ctx, gate := someoneElsesRun(uuid.New())
	for _, a := range []ai.ProposedAction{
		{ToolName: "read_doc", Params: map[string]string{"doc_uuid": uuid.NewString()}},
		{ToolName: "summarize_channel", Params: map[string]string{"channel_uuid": uuid.NewString()}},
		{ToolName: "search_workspace", Params: map[string]string{"query": "plan"}},
		// A tool from an admin-registered MCP server.
		{ToolName: "acme_lookup_invoice", Params: map[string]string{"id": "1"}},
	} {
		if got := gate.refusal(ctx, a); got == "" {
			t.Errorf("%s ran although whether the asker could reach it was never established", a.ToolName)
		}
	}
}

// The ratchet. A tool an agent can call must say what it touches before it can
// run for anyone but the sponsor; one added without a rule refuses, and this
// test names it so the rule gets written rather than the refusal discovered.
func TestEveryToolHasARuleForSomeoneElse(t *testing.T) {
	special := map[string]bool{codePRToolName: true, "read_poll": true, "run_analysis": true}
	for _, tool := range ai.ToolRegistry {
		if _, personal := personalTools[tool.Name]; personal || special[tool.Name] {
			continue
		}
		if _, known, _ := mcpServer.ResourceForToolCall(tool.Name, map[string]string{}); !known {
			t.Errorf("%s has no rule for a run someone other than the sponsor asked for: declare what it touches in "+
				"business/MCPServer (bridgedTools or inAppTools), or list it as personal in agentRequester.go", tool.Name)
		}
	}
}

func TestARunForTheSponsorHasNoGate(t *testing.T) {
	defer func(prev func(context.Context, string) (*dgraphStruct.DgraphUser, error)) { lookupPersonName = prev }(lookupPersonName)
	lookupPersonName = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{UserFullName: "Sana"}, nil
	}
	sponsor := uuid.NewString()
	if g := newRequesterGate(context.Background()); g != nil {
		t.Error("a run nobody asked for must have no gate")
	}
	if g := newRequesterGate(ai.WithRunRequester(context.Background(), sponsor, sponsor)); g != nil {
		t.Error("a run the sponsor asked for must have no gate")
	}
	g := newRequesterGate(ai.WithRunRequester(context.Background(), uuid.NewString(), sponsor))
	if g == nil || g.sponsorName != "Sana" {
		t.Fatalf("a run someone else asked for must be gated and name the sponsor, got %+v", g)
	}
	if got := g.refusal(context.Background(), ai.ProposedAction{ToolName: "send_dm"}); got != "only Sana can ask me to use their direct messages" {
		t.Errorf("got %q", got)
	}
}

func TestASponsorWhoCannotBeNamedIsDescribed(t *testing.T) {
	defer func(prev func(context.Context, string) (*dgraphStruct.DgraphUser, error)) { lookupPersonName = prev }(lookupPersonName)
	lookupPersonName = func(context.Context, string) (*dgraphStruct.DgraphUser, error) { return nil, errors.New("gone") }
	if got := personName(context.Background(), uuid.NewString(), "the person who set me up"); got != "the person who set me up" {
		t.Errorf("got %q", got)
	}
}

func TestATurnsRefusalsCoverOnlyCallsThatWouldRun(t *testing.T) {
	ctx, gate := someoneElsesRun(uuid.New())
	actions := []ai.ProposedAction{
		{ToolName: "gmail_search", Params: map[string]string{"query": "x"}},
		{ToolName: "gmail_search", Params: map[string]string{"query": "x"}}, // a repeat, decided once
		{ToolName: "gmail_send", Params: map[string]string{"to": "a@b.c"}},  // not on the allow-list
		{ToolName: blockerToolName, Params: map[string]string{"reason": "?"}},
	}
	got := gate.refusals(ctx, actions, map[string]bool{"gmail_search": true})
	if len(got) != 1 || got[ai.ActionSignature(actions[0])] == "" {
		t.Errorf("want exactly the allowed connector call refused, got %v", got)
	}
	var none *requesterGate
	if none.refusals(ctx, actions, map[string]bool{"gmail_search": true}) != nil {
		t.Error("a run for the sponsor refuses nothing")
	}
}

func TestARunIsJudgedAgainstItsOwnSponsor(t *testing.T) {
	asker, first, second := uuid.NewString(), uuid.New(), uuid.New()
	// Asked of the first agent, then handed to a second one owned by the asker.
	ctx := ai.WithRunRequester(context.Background(), asker, first.String())
	ctx = bindRunRequester(ctx, &model.AiAgent{CreatedBy: uuid.MustParse(asker)})
	if _, _, ok := ai.RunRequester(ctx); ok {
		t.Error("a run of the asker's own agent acts for them alone")
	}
	ctx = bindRunRequester(ai.WithRunRequester(context.Background(), asker, first.String()), &model.AiAgent{CreatedBy: second})
	if r, s, ok := ai.RunRequester(ctx); !ok || r != asker || s != second.String() {
		t.Errorf("got (%s, %s, %v), want the asker bound to the second agent's sponsor", r, s, ok)
	}
	if _, _, ok := ai.RunRequester(bindRunRequester(context.Background(), &model.AiAgent{CreatedBy: second})); ok {
		t.Error("binding a run nobody asked for must not invent an asker")
	}
}

func TestOnlyTheAskerOrTheSponsorMayContinueAJobInPlace(t *testing.T) {
	sponsor, asker, bystander := uuid.New(), uuid.New(), uuid.New()
	agent := &model.AiAgent{CreatedBy: sponsor}
	job := &model.AgentTask{TriggeredBy: &asker}
	cases := []struct {
		author string
		want   bool
	}{
		{asker.String(), true},
		{strings.ToUpper(asker.String()), true},
		{sponsor.String(), true},
		{"", false}, // nobody identified: from outside OneCamp, anyone (ProposePullRequestFeedback)
		{bystander.String(), false},
	}
	for _, c := range cases {
		if got := mayContinueInPlace(job, agent, c.author); got != c.want {
			t.Errorf("author %q: got %v, want %v", c.author, got, c.want)
		}
	}
	if mayContinueInPlace(&model.AgentTask{}, agent, bystander.String()) {
		t.Error("a job with no recorded asker is not anyone's to continue but the sponsor's")
	}
}

func TestAJobIsForWhoeverItRecordsAsAsking(t *testing.T) {
	asker := uuid.New()
	if got := jobRequester(&model.AgentTask{TriggeredBy: &asker}); got != asker.String() {
		t.Errorf("got %q", got)
	}
	nilID := uuid.Nil
	for _, job := range []*model.AgentTask{nil, {}, {TriggeredBy: &nilID}} {
		if got := jobRequester(job); got != "" {
			t.Errorf("a job with no asker must read as unidentified, got %q", got)
		}
	}
}

func TestARoutineIsRecordedAsTheAskersAndRunsForThem(t *testing.T) {
	sponsor, asker := uuid.New(), uuid.New()
	agent := &model.AiAgent{CreatedBy: sponsor}

	if by, err := routineCreator(context.Background(), agent); err != nil || by != sponsor {
		t.Errorf("a routine the sponsor sets up is theirs: got %s, %v", by, err)
	}
	ctx := ai.WithRunRequester(context.Background(), asker.String(), sponsor.String())
	if by, err := routineCreator(ctx, agent); err != nil || by != asker {
		t.Errorf("a routine someone else asks for is theirs: got %s, %v", by, err)
	}
	if _, err := routineCreator(ai.WithRunRequester(context.Background(), "", sponsor.String()), agent); err == nil {
		t.Error("a routine for an unidentified asker must not be set up at all")
	}

	run := routineRunFor(context.Background(), agent, &model.AgentRoutine{CreatedBy: asker})
	if r, _, ok := ai.RunRequester(run); !ok || r != asker.String() {
		t.Errorf("a routine someone else set up must run for them, got (%s, %v)", r, ok)
	}
	inherited := ai.WithRunRequester(context.Background(), asker.String(), sponsor.String())
	if _, _, ok := ai.RunRequester(routineRunFor(inherited, agent, &model.AgentRoutine{CreatedBy: sponsor})); ok {
		t.Error("a routine the sponsor set up runs for the sponsor, whatever the context carried")
	}
}

func TestACodeChangeIsForTheAskerAndApprovedByThem(t *testing.T) {
	sponsor, asker := uuid.New(), uuid.New()
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: sponsor}
	thread := Surface{Kind: SurfaceChannelPost, ChannelID: uuid.NewString(), PostID: uuid.NewString()}
	call := ai.ProposedAction{ToolName: codePRToolName, Params: map[string]string{
		"instruction": "fix it", ai.ProposalSurfaceParam: `{"kind":"channel_post","channel_id":"elsewhere"}`,
	}}

	// Nobody else asked: the sponsor approves, and it pushes as them.
	approver, params, err := codePRProposal(WithAgentRunSurface(context.Background(), thread), agent, call)
	if err != nil || approver != sponsor {
		t.Fatalf("got (%s, %v), want the sponsor", approver, err)
	}
	if got := DecodeSurface(params[ai.ProposalSurfaceParam]); got != thread {
		t.Errorf("the stamped thread is %+v, want the run's own %+v; a model-supplied one must be replaced", got, thread)
	}
	if params["instruction"] != "fix it" {
		t.Error("the call's own parameters must be kept")
	}

	// Someone else asked: they approve, and it would push as them.
	ctx := WithAgentRunSurface(ai.WithRunRequester(context.Background(), asker.String(), sponsor.String()), thread)
	if approver, _, err := codePRProposal(ctx, agent, call); err != nil || approver != asker {
		t.Errorf("got (%s, %v), want the asker", approver, err)
	}
	if who, err := codePRFor(ctx, sponsor.String()); err != nil || who != asker {
		t.Errorf("a change for someone else is theirs, got (%s, %v)", who, err)
	}
	if _, _, err := codePRProposal(ai.WithRunRequester(context.Background(), "", sponsor.String()), agent, call); err == nil {
		t.Error("a change for an unidentified asker must not be proposed to anyone")
	}
}

func TestOnlyAnApprovedProposalNamesTheThreadAndAgent(t *testing.T) {
	agentID := uuid.New()
	thread := Surface{Kind: SurfaceGroupChat, GroupID: "g-1", MessageID: uuid.NewString()}
	enc, _ := EncodeSurface(thread)
	call := ai.ProposedAction{ToolName: codePRToolName, Params: map[string]string{ai.ProposalSurfaceParam: enc}}

	// Sent to the assistant's own execute endpoint, the parameter is just a
	// parameter: it names no agent and no thread.
	if id, surface := codePRRunContext(context.Background(), call); id != uuid.Nil || surface.Kind != SurfaceTask {
		t.Errorf("an ordinary call took its thread from its parameters: (%s, %+v)", id, surface)
	}
	// Executing a proposal the agent made and a person approved, it is the one
	// place the parameter is the runner's own record.
	id, surface := codePRRunContext(ai.WithApprovedAgentProposal(context.Background(), agentID.String()), call)
	if id != agentID || surface != thread {
		t.Errorf("got (%s, %+v), want (%s, %+v)", id, surface, agentID, thread)
	}
}

func TestACodeChangeAlwaysWaitsForAPerson(t *testing.T) {
	t.Setenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN", "")
	for _, autonomy := range []string{model.AutonomyAuto, model.AutonomyApproval, model.AutonomyPlan} {
		if !codePRNeedsApproval(autonomy) {
			t.Errorf("code_pr ran without approval under %s autonomy", autonomy)
		}
	}
	// The deployment-wide opt-out applies to it as to every external effect, but
	// never to an autonomy level that asks for approval of every write.
	t.Setenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN", "true")
	if codePRNeedsApproval(model.AutonomyAuto) {
		t.Error("the opt-out should let full autonomy run it")
	}
	if !codePRNeedsApproval(model.AutonomyApproval) {
		t.Error("approval autonomy must still ask")
	}
}

// The working notes every run of an agent reads as its plan are written only
// by runs for its sponsor: offered to no other run, and refused if called anyway.
func TestOnlyTheSponsorsRunsKeepWorkingNotes(t *testing.T) {
	offers := func(specs []ai.ToolSpec) bool {
		for _, s := range specs {
			if s.Name == progressToolName {
				return true
			}
		}
		return false
	}
	for _, scoped := range []bool{false, true} {
		if !offers(nativeControlToolSpecs(scoped, true)) {
			t.Errorf("scoped=%v: a run for the sponsor is not offered save_progress", scoped)
		}
		if offers(nativeControlToolSpecs(scoped, false)) {
			t.Errorf("scoped=%v: a run for someone else is offered save_progress", scoped)
		}
	}
	_, gate := someoneElsesRun(uuid.New())
	if got := progressRefusal(gate); !strings.Contains(got, "Sana") || !strings.HasPrefix(got, "not saved") {
		t.Errorf("refusal %q does not say whose notes they are", got)
	}
}

// What a run sets up (a routine, a remembered instruction) is recorded for the
// person it is for: the sponsor when nobody else asked, else whoever did, and
// nobody when they could not be identified.
func TestWhatARunSetsUpIsRecordedForWhoeverAsked(t *testing.T) {
	sponsor, asker := uuid.New(), uuid.New()
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: sponsor}
	cases := []struct {
		name      string
		ctx       context.Context
		want      uuid.UUID
		wantKnown bool
	}{
		{"nobody asked", ai.WithoutRunRequester(context.Background()), sponsor, true},
		{"the sponsor asked", ai.WithRunRequester(context.Background(), sponsor.String(), sponsor.String()), sponsor, true},
		{"someone else asked", ai.WithRunRequester(context.Background(), asker.String(), sponsor.String()), asker, true},
		{"someone unidentified asked", ai.WithRunRequester(context.Background(), "", sponsor.String()), uuid.Nil, false},
		{"someone with no id asked", ai.WithRunRequester(context.Background(), uuid.Nil.String(), sponsor.String()), uuid.Nil, false},
	}
	for _, c := range cases {
		who, known := askerOf(c.ctx, agent)
		if known != c.wantKnown || (known && who != c.want) {
			t.Errorf("%s: %s known=%v, want %s known=%v", c.name, who, known, c.want, c.wantKnown)
		}
	}
}

// workspaceWideTools are the tools that name no object, so the gate can only
// check that the person who asked may ask for work at all
// (mcpServer.ResourceWorkspace), with how each keeps a run for someone else to
// what that person can see. A tool that becomes workspace-wide must say how it
// is narrowed before it can be added here: nothing else would.
var workspaceWideTools = map[string]string{
	"search_workspace":  "the index ANDs the asker's own permission filter (services/AI runRequester.go); docs are re-read for both (business/AI docHits.go); the sponsor's connected accounts are left out (unifiedSearch.go)",
	"list_projects":     "only projects the asker is in (business/AI requesterReach.go askerView)",
	"list_teams":        "only teams the asker is in (askerView)",
	"list_tasks":        "only tasks in projects the asker is in (askerView)",
	"list_tables":       "only tables the asker may view (askerTables), and each table read opens for both (dataTableBusiness.AlsoFor)",
	"list_data_sources": "only data sources the asker may query (askerDataSources)",
	"find_people":       "not narrowed: the people directory, which every member sees",
	"web_search":        "not narrowed: reads nothing of the workspace's",
	// The code tools read through the workspace's GitHub integration: the
	// connecting admin's own token, for any owner/repo named, the same for
	// every member and every run. Not the sponsor's reach, and not narrowed.
	"code_analyze":        "not narrowed: the workspace's GitHub integration, the same for everyone",
	"repo_summary":        "not narrowed: the workspace's GitHub integration, the same for everyone",
	"list_recent_changes": "not narrowed: the workspace's GitHub integration, the same for everyone",
	"list_commits":        "not narrowed: the workspace's GitHub integration, the same for everyone",
	"read_repo_file":      "not narrowed: the workspace's GitHub integration, the same for everyone",
	"search_repo_code":    "not narrowed: the workspace's GitHub integration, the same for everyone",
}

func TestEveryWorkspaceWideToolSaysHowItIsNarrowed(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range ai.ToolRegistry {
		ref, known, err := mcpServer.ResourceForToolCall(tool.Name, map[string]string{})
		if !known || err != nil || ref.Kind != mcpServer.ResourceWorkspace {
			continue
		}
		seen[tool.Name] = true
		if strings.TrimSpace(workspaceWideTools[tool.Name]) == "" {
			t.Errorf("%s names no object, so a run for someone else reaches whatever it returns: narrow it to what "+
				"the asker can see and say how in workspaceWideTools", tool.Name)
		}
	}
	for tool := range workspaceWideTools {
		if !seen[tool] {
			t.Errorf("%s is listed as workspace-wide but no longer is (or is gone); take it out of workspaceWideTools", tool)
		}
	}
}

// A public channel the asker hasn't joined is refused like any channel they
// aren't in, but the refusal says what would change it rather than claim the
// channel is beyond them.
func TestAPublicChannelTheAskerHasntJoinedSaysSo(t *testing.T) {
	_, gate := someoneElsesRun(uuid.New())
	restore := channelIsPublic
	t.Cleanup(func() { channelIsPublic = restore })
	public := map[string]bool{"town-square": true}
	channelIsPublic = func(_ context.Context, _, channelID string) (bool, error) {
		if channelID == "broken" {
			return false, errors.New("graph down")
		}
		return public[channelID], nil
	}
	ref := func(id string) mcpServer.ResourceRef {
		return mcpServer.ResourceRef{Kind: mcpServer.ResourceChannel, ID: id, Access: mcpServer.AccessRead}
	}
	got := gate.reachRefusal(context.Background(), ref("town-square"), mcpServer.ReasonNotChannelMember)
	if !strings.Contains(got, "hasn't joined this channel") || !strings.Contains(got, "Sana") || strings.Contains(got, "outside what") {
		t.Errorf("a public channel: %q", got)
	}
	for _, id := range []string{"exec", "broken"} {
		if got := gate.reachRefusal(context.Background(), ref(id), mcpServer.ReasonNotChannelMember); !strings.Contains(got, "they're not a member of this channel") {
			t.Errorf("%s: %q", id, got)
		}
	}
}

// Someone told a job isn't theirs to steer hears whose it is: the asker's and
// the sponsor's, or, for the sponsor's own job, the sponsor's alone.
func TestANoteSaysWhoseJobItIs(t *testing.T) {
	sponsor, asker := uuid.New(), uuid.New()
	defer func(prev func(context.Context, string) (*dgraphStruct.DgraphUser, error)) { lookupPersonName = prev }(lookupPersonName)
	names := map[string]string{sponsor.String(): "Sana", asker.String(): "Ravi"}
	lookupPersonName = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{UserFullName: names[id]}, nil
	}
	agent := &model.AiAgent{CreatedBy: sponsor}
	if got := notYourJobNote(context.Background(), agent, &model.AgentTask{TriggeredBy: &asker}); !strings.Contains(got, "for Ravi, so I can only take instructions on it from them or from Sana.") {
		t.Errorf("someone else's job: %q", got)
	}
	if got := notYourJobNote(context.Background(), agent, &model.AgentTask{TriggeredBy: &sponsor}); !strings.Contains(got, "for Sana, so I can only take instructions on it from them.") || strings.Contains(got, "or from") {
		t.Errorf("the sponsor's job: %q", got)
	}
	// A job with no recorded asker is nobody's: only the sponsor may add to it.
	if got := notYourJobNote(context.Background(), agent, &model.AgentTask{}); !strings.Contains(got, "only take instructions on it from Sana") || strings.Contains(got, "working on this for") {
		t.Errorf("a job with no recorded asker: %q", got)
	}
}
