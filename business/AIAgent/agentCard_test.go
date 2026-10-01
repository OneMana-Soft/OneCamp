package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// The card is read by every member, so the tests are about two things: what it
// must never carry, and whether its numbers are the week's and nothing else.

func cardFixture() *model.AiAgent {
	desc := "  Answers release questions  "
	return &model.AiAgent{
		Id:           uuid.New(),
		Name:         "Release Captain",
		Description:  &desc,
		Instructions: "SECRET INSTRUCTIONS",
		EnabledTools: `["search_messages","create_task"]`,
		Scope:        `{"channel_ids":["c1","c2"],"project_ids":[]}`,
		IsActive:     true,
		DmAble:       true,
		Autonomy:     model.AutonomyApproval,
		CreatedBy:    uuid.New(),
		AGUIEndpoint: "https://brain.internal/agui",
	}
}

func stubCard(t *testing.T, a *model.AiAgent, runs []*model.AgentRun, inv map[string]*model.AgentActivity, now time.Time) {
	t.Helper()
	oldBot, oldRuns, oldInv, oldName, oldNow := cardAgentByBot, cardRuns, cardInventory, cardSponsorName, cardNow
	t.Cleanup(func() {
		cardAgentByBot, cardRuns, cardInventory, cardSponsorName, cardNow = oldBot, oldRuns, oldInv, oldName, oldNow
	})
	cardAgentByBot = func(context.Context, uuid.UUID) (*model.AiAgent, error) { return a, nil }
	cardRuns = func(context.Context, uuid.UUID, int) ([]*model.AgentRun, error) { return runs, nil }
	cardInventory = func(context.Context, time.Time) (map[string]*model.AgentActivity, map[string]*model.AgentActivity, error) {
		return inv, nil, nil
	}
	cardSponsorName = func(context.Context, uuid.UUID) string { return "Priya N" }
	cardNow = func() time.Time { return now }
}

func steps(t *testing.T, calls ...toolCallRecord) string {
	t.Helper()
	b, err := json.Marshal([]stepRecord{{Iteration: 1, ToolCalls: calls}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAgentCardCarriesNothingPrivate(t *testing.T) {
	a := cardFixture()
	stubCard(t, a, nil, nil, time.Now())
	card, err := GetAgentCard(context.Background(), uuid.New(), Actor{UserID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(card)
	for _, secret := range []string{"SECRET INSTRUCTIONS", "brain.internal", `"c1"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("card leaks %q: %s", secret, raw)
		}
	}
	if card.Description != "Answers release questions" || card.Sponsor != "Priya N" {
		t.Fatalf("description/sponsor = %q/%q", card.Description, card.Sponsor)
	}
	if card.ScopedChannels != 2 || card.ScopedProjects != 0 || len(card.Tools) != 2 {
		t.Fatalf("scope/tools = %d/%d/%v", card.ScopedChannels, card.ScopedProjects, card.Tools)
	}
	if card.CanManage {
		t.Fatal("a stranger must not be offered management")
	}
}

func TestAgentCardCanManageForSponsorAndAdmin(t *testing.T) {
	a := cardFixture()
	stubCard(t, a, nil, nil, time.Now())
	for name, actor := range map[string]Actor{"sponsor": {UserID: a.CreatedBy}, "admin": {UserID: uuid.New(), IsAdmin: true}} {
		card, err := GetAgentCard(context.Background(), uuid.New(), actor)
		if err != nil || !card.CanManage {
			t.Fatalf("%s: can_manage=%v err=%v", name, card != nil && card.CanManage, err)
		}
	}
}

func TestAgentCardCountsOnlyTheWeek(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	a := cardFixture()
	refused := toolCallRecord{Tool: "post_message", Governance: "permission_denied"}
	asked := toolCallRecord{Tool: "create_task", Governance: govApprovalRequired}
	runs := []*model.AgentRun{
		{StartedAt: now.Add(-time.Hour), Steps: steps(t, refused, refused, asked)},
		{StartedAt: now.Add(-48 * time.Hour), Steps: steps(t, toolCallRecord{Tool: "search_messages"})},
		{StartedAt: now.Add(-8 * 24 * time.Hour), Steps: steps(t, refused)}, // outside the week
	}
	inv := map[string]*model.AgentActivity{a.Id.String(): {Actions: 5, Refusals: 1}}
	stubCard(t, a, runs, inv, now)

	card, err := GetAgentCard(context.Background(), uuid.New(), Actor{UserID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	// Two refused calls in-product (approval-required is not a refusal) plus one over MCP.
	if card.Runs != 2 || card.Refusals != 3 || card.Actions != 5 || card.WindowDays != 7 {
		t.Fatalf("runs=%d refusals=%d actions=%d window=%d", card.Runs, card.Refusals, card.Actions, card.WindowDays)
	}
	if card.LastRunAt == nil || !card.LastRunAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("last run = %v", card.LastRunAt)
	}
}

func TestAgentCardNotAnAgent(t *testing.T) {
	stubCard(t, nil, nil, nil, time.Now())
	if _, err := GetAgentCard(context.Background(), uuid.New(), Actor{}); !IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestMemberNameNeverAnAddress(t *testing.T) {
	cases := []struct {
		u    *dgraphStruct.DgraphUser
		want string
	}{
		{&dgraphStruct.DgraphUser{UserFullName: "Maya Chen", UserName: "maya"}, "Maya Chen"},
		{&dgraphStruct.DgraphUser{UserName: "maya"}, "maya"},
		{&dgraphStruct.DgraphUser{UserName: "maya.chen@cast.demo.onemana.dev"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := memberName(c.u); got != c.want {
			t.Errorf("memberName(%+v) = %q, want %q", c.u, got, c.want)
		}
	}
}

func stubChannelAgents(t *testing.T, member bool, bots []uuid.UUID, agents []*model.AiAgent) {
	t.Helper()
	oldM, oldA := channelMembersFor, agentsByBots
	t.Cleanup(func() { channelMembersFor, agentsByBots = oldM, oldA })
	channelMembersFor = func(context.Context, uuid.UUID, string) (bool, []uuid.UUID, error) { return member, bots, nil }
	agentsByBots = func(_ context.Context, ids []uuid.UUID) ([]*model.AiAgent, error) {
		if len(ids) != len(bots) {
			t.Fatalf("asked for %d bots, channel has %d", len(ids), len(bots))
		}
		return agents, nil
	}
}

func TestChannelAgentsOnlyForMembers(t *testing.T) {
	stubChannelAgents(t, false, nil, nil)
	if _, err := ChannelAgents(context.Background(), uuid.New(), Actor{}); !IsForbidden(err) {
		t.Fatalf("err = %v, want forbidden for a non-member", err)
	}
}

func TestChannelAgentsNamesTheAgentsPresent(t *testing.T) {
	bot := uuid.New()
	desc := "  Keeps the checklist honest  "
	stubChannelAgents(t, true, []uuid.UUID{bot}, []*model.AiAgent{
		{Name: "Release Captain", BotUserId: &bot, Description: &desc, Instructions: "SECRET"},
		{Name: "No account yet"}, // not provisioned: cannot be opened, so not listed
	})
	got, err := ChannelAgents(context.Background(), uuid.New(), Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].BotUserID != bot || got[0].Description != "Keeps the checklist honest" {
		t.Fatalf("got %+v", got)
	}
}
