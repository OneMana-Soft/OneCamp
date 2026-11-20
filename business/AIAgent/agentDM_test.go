package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// synthDMPrompt must embed the member's message and instruct the agent to write
// the reply as its final answer (it is posted as the agent in the DM, so the
// agent must not use a send-message tool to reply).
func TestSynthDMPrompt(t *testing.T) {
	p := synthDMPrompt("  what's blocking the launch?  ")
	if !strings.Contains(p, "what's blocking the launch?") {
		t.Fatalf("prompt missing the trimmed message:\n%s", p)
	}
	if !strings.Contains(p, "1:1 DM") {
		t.Fatalf("prompt should frame it as a direct message:\n%s", p)
	}
	if !strings.Contains(p, "do NOT use a send-message tool") {
		t.Fatalf("prompt should tell the agent not to send-message itself:\n%s", p)
	}
}

// dmWorld is three DM-able agents, each with its bot, and the receivers a DM
// can have: the counts are of the agent lists read and the bots ensured.
type dmWorld struct {
	agents        []*model.AiAgent
	bots          map[uuid.UUID]*userBusiness.BotIdentity
	people        map[string]*dgraphStruct.DgraphUser
	readErr       error
	lists, ensure int
}

func withDMWorld(t *testing.T) *dmWorld {
	t.Helper()
	w := &dmWorld{bots: map[uuid.UUID]*userBusiness.BotIdentity{}, people: map[string]*dgraphStruct.DgraphUser{}}
	for _, name := range []string{"Scout", "Ledger", "Pixel"} {
		a := &model.AiAgent{Id: uuid.New(), Name: name}
		bot := &userBusiness.BotIdentity{UserID: uuid.New(), DgraphUID: "0x" + name, Name: name}
		bot.UUID = bot.UserID.String()
		w.agents, w.bots[a.Id] = append(w.agents, a), bot
		w.people[bot.UUID] = &dgraphStruct.DgraphUser{Uid: bot.DgraphUID, Uuid: bot.UUID, IsBot: true}
	}
	pl, pe, pr := listDMAble, ensureAgentBot, readReceiver
	t.Cleanup(func() { listDMAble, ensureAgentBot, readReceiver = pl, pe, pr })
	listDMAble = func(context.Context) ([]*model.AiAgent, error) {
		w.lists++
		return w.agents, nil
	}
	ensureAgentBot = func(_ context.Context, id uuid.UUID, _, _ string) (*userBusiness.BotIdentity, error) {
		w.ensure++
		return w.bots[id], nil
	}
	readReceiver = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		if w.readErr != nil {
			return nil, w.readErr
		}
		if u, ok := w.people[id]; ok {
			return u, nil
		}
		return nil, dgexec.ErrNotFound
	}
	return w
}

// A DM to a person is no agent's: it reads the receiver once and provisions
// no agent's bot (today every DM-able agent's, on every DM).
func TestADMToAPersonProvisionsNoAgentBot(t *testing.T) {
	w := withDMWorld(t)
	human := uuid.NewString()
	w.people[human] = &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: human}
	agent, bot, err := AgentForDMPrincipal(context.Background(), human)
	if err != nil || agent != nil || bot != nil {
		t.Fatalf("a DM to a person found %v %v %v", agent, bot, err)
	}
	if w.lists != 0 || w.ensure != 0 {
		t.Errorf("a DM to a person listed the agents %d times and ensured %d bots, want none", w.lists, w.ensure)
	}
	if agent, _, _ := AgentForDMPrincipal(context.Background(), uuid.NewString()); agent != nil || w.ensure != 0 {
		t.Errorf("a DM to no one found %v and ensured %d bots", agent, w.ensure)
	}
}

// A DM to an agent's bot still finds its agent, by the bot's uuid or its
// graph uid, and so does one whose receiver couldn't be read; a bot that is
// no agent's (an integration's) finds none.
func TestADMToAnAgentsBotStillFindsItsAgent(t *testing.T) {
	w := withDMWorld(t)
	ledger := w.bots[w.agents[1].Id]
	for _, id := range []string{ledger.UUID, ledger.DgraphUID} {
		agent, bot, err := AgentForDMPrincipal(context.Background(), id)
		if err != nil || agent != w.agents[1] || bot != ledger {
			t.Errorf("a DM to %s found %v %v %v, want Ledger", id, agent, bot, err)
		}
	}
	w.readErr = errors.New("graph away")
	if agent, _, _ := AgentForDMPrincipal(context.Background(), ledger.UUID); agent != w.agents[1] {
		t.Errorf("with the receiver unread, a DM to Ledger's bot found %v", agent)
	}
	w.readErr = nil
	other := uuid.NewString()
	w.people[other] = &dgraphStruct.DgraphUser{Uid: "0x8", Uuid: other, IsBot: true}
	if agent, _, _ := AgentForDMPrincipal(context.Background(), other); agent != nil {
		t.Errorf("a DM to an integration's bot found %v", agent)
	}
}
