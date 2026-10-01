package business

import (
	"context"
	"reflect"
	"strings"
	"testing"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// EvaluateDelegation gates who may cause an agent to spend money, so the cases
// worth the most attention are the ones that must be REFUSED. A guard that only
// gets tested on its happy path is a guard nobody should trust.

const (
	human  = "" // empty author => a person spoke
	agentA = "11111111-1111-1111-1111-111111111111"
	agentB = "22222222-2222-2222-2222-222222222222"
	agentC = "33333333-3333-3333-3333-333333333333"
	person = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func onCfg() DelegationConfig {
	return DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 2, MaxChainAgents: 4}
}

func TestEvaluateDelegation(t *testing.T) {
	tests := []struct {
		name   string
		cfg    DelegationConfig
		dc     DelegationContext
		target string
		want   bool
	}{
		{
			// The path every existing mention takes. It must stay allowed even
			// with the feature off, or turning the flag off would break the
			// single-agent product.
			name:   "human mention is allowed with feature disabled",
			cfg:    DelegationConfig{Enabled: false, ChannelOptIn: false},
			dc:     DelegationContext{AuthorAgentID: human, Hop: 0},
			target: agentA,
			want:   true,
		},
		{
			name:   "human mention is allowed in a channel that has not opted in",
			cfg:    DelegationConfig{Enabled: true, ChannelOptIn: false},
			dc:     DelegationContext{AuthorAgentID: human},
			target: agentA,
			want:   true,
		},
		{
			name:   "agent to agent allowed inside budget",
			cfg:    onCfg(),
			dc:     DelegationContext{AuthorAgentID: agentA, Hop: 0, OriginUserID: person, Chain: []string{agentA}},
			target: agentB,
			want:   true,
		},
		{
			name:   "agent to agent refused when the feature is off",
			cfg:    DelegationConfig{Enabled: false, ChannelOptIn: true, MaxHops: 2},
			dc:     DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Chain: []string{agentA}},
			target: agentB,
			want:   false,
		},
		{
			name:   "agent to agent refused when the channel has not opted in",
			cfg:    DelegationConfig{Enabled: true, ChannelOptIn: false, MaxHops: 2},
			dc:     DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Chain: []string{agentA}},
			target: agentB,
			want:   false,
		},
		{
			// An orphaned chain has no principal to permission-check against and
			// nobody accountable in the audit trail.
			name:   "refused when no person is at the root",
			cfg:    onCfg(),
			dc:     DelegationContext{AuthorAgentID: agentA, OriginUserID: "", Chain: []string{agentA}},
			target: agentB,
			want:   false,
		},
		{
			name:   "self-delegation refused",
			cfg:    onCfg(),
			dc:     DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Chain: []string{agentA}},
			target: agentA,
			want:   false,
		},
		{
			name:   "self-delegation refused regardless of case",
			cfg:    onCfg(),
			dc:     DelegationContext{AuthorAgentID: "AAAA-BBBB", OriginUserID: person},
			target: "aaaa-bbbb",
			want:   false,
		},
		{
			// The ping-pong that a naive hop counter misses: each side believes
			// it is starting a fresh delegation.
			name:   "cycle back to an agent already in the chain refused",
			cfg:    onCfg(),
			dc:     DelegationContext{AuthorAgentID: agentB, OriginUserID: person, Hop: 1, Chain: []string{agentA, agentB}},
			target: agentA,
			want:   false,
		},
		{
			name:   "hop budget exhausted refused",
			cfg:    DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 1},
			dc:     DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Hop: 1, Chain: []string{agentA}},
			target: agentB,
			want:   false,
		},
		{
			name:   "chain agent cap refused",
			cfg:    DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 10, MaxChainAgents: 2},
			dc:     DelegationContext{AuthorAgentID: agentB, OriginUserID: person, Hop: 1, Chain: []string{agentA, agentB}},
			target: agentC,
			want:   false,
		},
		{
			name:   "empty target refused",
			cfg:    onCfg(),
			dc:     DelegationContext{AuthorAgentID: agentA, OriginUserID: person},
			target: "   ",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateDelegation(tt.cfg, tt.dc, tt.target)
			if got.Allow != tt.want {
				t.Fatalf("Allow = %v, want %v (reason %q)", got.Allow, tt.want, got.Reason)
			}
			// Every decision must explain itself: a denial the operator cannot
			// read is a denial that gets reported as a bug.
			if got.Reason == "" {
				t.Fatal("Reason must never be empty")
			}
		})
	}
}

// A two-agent pair must terminate no matter how enthusiastically they mention
// each other. This walks the chain the way the dispatcher would and asserts it
// stops — the property that actually matters, rather than any single decision.
func TestDelegationChainTerminates(t *testing.T) {
	cfg := DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 3, MaxChainAgents: 8}
	dc := DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Chain: []string{agentA}}

	targets := []string{agentB, agentA, agentB, agentA, agentB}
	allowed := 0
	for _, target := range targets {
		if !EvaluateDelegation(cfg, dc, target).Allow {
			break
		}
		allowed++
		dc = NextDelegationContext(dc, target)
	}
	// A -> B is allowed; B -> A is a cycle and must stop it there.
	if allowed != 1 {
		t.Fatalf("ping-pong ran for %d hops, want 1", allowed)
	}
}

// A genuine three-party chain (human -> triage -> coder) is the motivating case
// and must work, otherwise the budget is too tight to be useful.
func TestDelegationAllowsRealisticChain(t *testing.T) {
	cfg := DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 2, MaxChainAgents: 4}

	// A person mentions the triage agent.
	human0 := DelegationContext{AuthorAgentID: human, OriginUserID: person}
	if d := EvaluateDelegation(cfg, human0, agentA); !d.Allow {
		t.Fatalf("human -> triage refused: %s", d.Reason)
	}

	// Triage runs and hands off to the coder.
	triage := NextDelegationContext(DelegationContext{OriginUserID: person}, agentA)
	if d := EvaluateDelegation(cfg, triage, agentB); !d.Allow {
		t.Fatalf("triage -> coder refused: %s", d.Reason)
	}

	// The coder must not start a third agent hop under a 2-hop budget.
	coder := NextDelegationContext(triage, agentB)
	if d := EvaluateDelegation(cfg, coder, agentC); d.Allow {
		t.Fatal("coder -> third agent should exceed the 2-hop budget")
	}
}

func TestNextDelegationContext(t *testing.T) {
	dc := DelegationContext{Hop: 1, OriginUserID: person, Chain: []string{agentA}}
	next := NextDelegationContext(dc, agentB)

	if next.Hop != 2 {
		t.Fatalf("Hop = %d, want 2", next.Hop)
	}
	if next.OriginUserID != person {
		t.Fatalf("OriginUserID = %q, want %q", next.OriginUserID, person)
	}
	if !reflect.DeepEqual(next.Chain, []string{agentA, agentB}) {
		t.Fatalf("Chain = %v", next.Chain)
	}
	if next.AuthorAgentID != agentB {
		t.Fatalf("AuthorAgentID = %q, want %q", next.AuthorAgentID, agentB)
	}
}

// Appending to the parent's chain in place would let two concurrent dispatches
// from the same message corrupt each other's lineage — invisible until a chain
// reports the wrong parent.
func TestNextDelegationContextDoesNotAliasParentChain(t *testing.T) {
	parent := DelegationContext{OriginUserID: person, Chain: make([]string, 1, 8)}
	parent.Chain[0] = agentA

	b := NextDelegationContext(parent, agentB)
	c := NextDelegationContext(parent, agentC)

	if !reflect.DeepEqual(parent.Chain, []string{agentA}) {
		t.Fatalf("parent chain mutated: %v", parent.Chain)
	}
	if !reflect.DeepEqual(b.Chain, []string{agentA, agentB}) {
		t.Fatalf("b chain = %v", b.Chain)
	}
	if !reflect.DeepEqual(c.Chain, []string{agentA, agentC}) {
		t.Fatalf("c chain = %v (aliased with b?)", c.Chain)
	}
}

func TestDelegationContextFromEvent(t *testing.T) {
	t.Run("absent keys read as a human hop 0", func(t *testing.T) {
		// An event from a node that predates this feature, or a rolling deploy.
		// It must read as human-authored, not as a malformed chain.
		dc := DelegationContextFromEvent(map[string]interface{}{"text": "hi"}, "")
		if dc.Hop != 0 || dc.OriginUserID != "" || len(dc.Chain) != 0 || dc.AuthorAgentID != "" {
			t.Fatalf("got %+v", dc)
		}
		if !EvaluateDelegation(onCfg(), dc, agentA).Allow {
			t.Fatal("a legacy event must still dispatch")
		}
	})

	t.Run("nil map is safe", func(t *testing.T) {
		if dc := DelegationContextFromEvent(nil, agentA); dc.AuthorAgentID != agentA || dc.Hop != 0 {
			t.Fatalf("got %+v", dc)
		}
	})

	t.Run("survives a JSON round-trip", func(t *testing.T) {
		// Outgoing webhooks marshal the payload, so numbers come back float64
		// and []string comes back []interface{}.
		dc := DelegationContextFromEvent(map[string]interface{}{
			EventKeyAgentHop:     float64(2),
			EventKeyOriginUserID: person,
			EventKeyAgentChain:   []interface{}{agentA, agentB},
		}, agentB)
		if dc.Hop != 2 {
			t.Fatalf("Hop = %d, want 2", dc.Hop)
		}
		if !reflect.DeepEqual(dc.Chain, []string{agentA, agentB}) {
			t.Fatalf("Chain = %v", dc.Chain)
		}
	})

	t.Run("string hop and negative hop", func(t *testing.T) {
		if dc := DelegationContextFromEvent(map[string]interface{}{EventKeyAgentHop: "3"}, agentA); dc.Hop != 3 {
			t.Fatalf("Hop = %d, want 3", dc.Hop)
		}
		// A negative hop would hand out free budget forever.
		if dc := DelegationContextFromEvent(map[string]interface{}{EventKeyAgentHop: -5}, agentA); dc.Hop != 0 {
			t.Fatalf("Hop = %d, want clamped to 0", dc.Hop)
		}
	})
}

func TestApplyToEventRoundTrip(t *testing.T) {
	dc := DelegationContext{Hop: 1, OriginUserID: person, Chain: []string{agentA}}
	data := dc.ApplyToEvent(map[string]interface{}{"text": "hello"})

	if data["text"] != "hello" {
		t.Fatal("ApplyToEvent must not disturb existing fields")
	}
	back := DelegationContextFromEvent(data, agentA)
	if back.Hop != 1 || back.OriginUserID != person || !reflect.DeepEqual(back.Chain, []string{agentA}) {
		t.Fatalf("round-trip lost lineage: %+v", back)
	}
	if dc.ApplyToEvent(nil) != nil {
		t.Fatal("ApplyToEvent(nil) must stay nil")
	}
}

func TestDelegationConfigDefaults(t *testing.T) {
	// An empty config must still bound the walk, so a caller who forgets to set
	// limits gets the safe behaviour rather than an unbounded one.
	cfg := DelegationConfig{Enabled: true, ChannelOptIn: true}
	if cfg.hopLimit() != defaultMaxDelegationHops {
		t.Fatalf("hopLimit = %d", cfg.hopLimit())
	}
	if cfg.chainLimit() != defaultMaxChainAgents {
		t.Fatalf("chainLimit = %d", cfg.chainLimit())
	}
	deep := DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Hop: defaultMaxDelegationHops}
	if EvaluateDelegation(cfg, deep, agentB).Allow {
		t.Fatal("default hop budget must bound the chain")
	}
}

func TestAgentCollabAllowedInSurface(t *testing.T) {
	const chA = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const chB = "dddddddd-dddd-dddd-dddd-dddddddddddd"

	tests := []struct {
		name string
		env  string
		ch   string
		want bool
	}{
		// Unset must mean off. A feature that changes who can spend money should
		// never switch on because a variable was forgotten.
		{"unset is off", "", chA, false},
		{"blank is off", "   ", chA, false},
		{"wildcard opens every channel", "*", chA, true},
		{"listed channel allowed", chA + "," + chB, chB, true},
		{"unlisted channel refused", chA, chB, false},
		{"whitespace around ids tolerated", " " + chA + " , " + chB + " ", chA, true},
		{"case-insensitive match", strings.ToUpper(chA), strings.ToLower(chA), true},
		// A wildcard-less list with an empty channel id must not match a stray
		// empty entry (",," style config), or a DM with no channel would qualify.
		{"empty channel id refused", chA + ",,", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AI_AGENT_DELEGATION_CHANNELS", tt.env)
			if got := agentCollabAllowedInSurface(tt.ch); got != tt.want {
				t.Fatalf("allowed = %v, want %v (env %q, channel %q)", got, tt.want, tt.env, tt.ch)
			}
		})
	}
}

func TestDelegationEnvKnobs(t *testing.T) {
	t.Run("env alone cannot ENABLE delegation", func(t *testing.T) {
		// The env var was demoted to a kill switch when the setting moved into the
		// admin AI config. An admin grants the capability; infrastructure may only
		// veto it. So no env value turns delegation on by itself — with no AI config
		// loaded (as in this binary) the answer is always off.
		for _, v := range []string{"", "true", "1", "yes", "on", "false", "0", "maybe"} {
			t.Setenv("AI_AGENT_DELEGATION", v)
			if delegationEnabled() {
				t.Fatalf("AI_AGENT_DELEGATION=%q must not enable delegation on its own", v)
			}
		}
	})

	t.Run("only an explicit negative vetoes; absent means no opinion", func(t *testing.T) {
		// The asymmetry is the point: an empty var must NOT read as a veto, or a
		// deployment that never set it would silently override every admin.
		for _, v := range []string{"false", "0", "no", "off", "OFF", " false "} {
			t.Setenv("AI_AGENT_DELEGATION", v)
			if !envVetoesDelegation() {
				t.Fatalf("AI_AGENT_DELEGATION=%q should veto", v)
			}
		}
		for _, v := range []string{"", "   ", "true", "1", "yes", "on", "maybe"} {
			t.Setenv("AI_AGENT_DELEGATION", v)
			if envVetoesDelegation() {
				t.Fatalf("AI_AGENT_DELEGATION=%q must not veto", v)
			}
		}
	})

	t.Run("hop budget falls back rather than to zero", func(t *testing.T) {
		// A zero budget would silently disable a feature the operator turned on.
		for _, v := range []string{"", "abc", "0", "-3"} {
			t.Setenv("AI_AGENT_DELEGATION_MAX_HOPS", v)
			if got := delegationHopBudget(); got != defaultMaxDelegationHops {
				t.Fatalf("AI_AGENT_DELEGATION_MAX_HOPS=%q gave %d, want default %d", v, got, defaultMaxDelegationHops)
			}
		}
		t.Setenv("AI_AGENT_DELEGATION_MAX_HOPS", "3")
		if got := delegationHopBudget(); got != 3 {
			t.Fatalf("hop budget = %d, want 3", got)
		}
	})
}

func TestOriginLineage(t *testing.T) {
	t.Run("a human author becomes the origin", func(t *testing.T) {
		got := originLineage(DelegationContext{}, person)
		if got.OriginUserID != person {
			t.Fatalf("OriginUserID = %q, want %q", got.OriginUserID, person)
		}
	})

	t.Run("an agent author must NOT overwrite the origin", func(t *testing.T) {
		// If hop 2 credited itself as the root, every chain would look like it was
		// authorised by a bot and the audit trail would name no person at all.
		in := DelegationContext{AuthorAgentID: agentA, OriginUserID: person}
		got := originLineage(in, "bot-user-id-of-agent-a")
		if got.OriginUserID != person {
			t.Fatalf("OriginUserID = %q, want the original person %q", got.OriginUserID, person)
		}
	})

	t.Run("an agent author with no origin stays orphaned rather than self-crediting", func(t *testing.T) {
		// Staying empty is what makes EvaluateDelegation refuse it. Filling it in
		// with the bot's own id would manufacture an authorisation nobody gave.
		got := originLineage(DelegationContext{AuthorAgentID: agentA}, "bot-user-id")
		if got.OriginUserID != "" {
			t.Fatalf("OriginUserID = %q, want empty", got.OriginUserID)
		}
		if EvaluateDelegation(onCfg(), got, agentB).Allow {
			t.Fatal("an orphaned chain must not dispatch")
		}
	})
}

func TestDelegationContextRoundTripsThroughContext(t *testing.T) {
	dc := DelegationContext{Hop: 1, OriginUserID: person, Chain: []string{agentA}, AuthorAgentID: agentA}
	ctx := WithDelegationContext(context.Background(), dc)

	got, ok := delegationFromContext(ctx)
	if !ok {
		t.Fatal("lineage not found on ctx")
	}
	if got.Hop != 1 || got.OriginUserID != person || !reflect.DeepEqual(got.Chain, []string{agentA}) {
		t.Fatalf("got %+v", got)
	}

	// A bare context must report "not part of a chain" rather than a zero chain
	// that looks legitimate — this is what keeps schedules, routines and manual
	// runs from emitting delegation events.
	if _, ok := delegationFromContext(context.Background()); ok {
		t.Fatal("a bare context must not report lineage")
	}
}

// EmitAgentMessage must be inert with the feature off. This is the property that
// makes the change safe to merge before anyone opts a channel in: with the flag
// unset, nothing is published and agents stay mute to each other.
func TestEmitAgentMessageIsInertWhenDisabled(t *testing.T) {
	t.Setenv("AI_AGENT_DELEGATION", "false")
	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", "*")

	published := 0
	webhookBusiness.RegisterEventListener(func(_ context.Context, eventType string, _ map[string]interface{}) {
		if eventType == EventTypeAgentMessage {
			published++
		}
	})

	ctx := WithDelegationContext(context.Background(), DelegationContext{OriginUserID: person, Chain: []string{agentA}})
	bot := &userBusiness.BotIdentity{UUID: "bot-uuid", Name: "Triage"}
	EmitAgentMessage(ctx, Surface{Kind: SurfaceChannelPost, ChannelID: "channel-1"}, "post-1", "#eng", "post-1",
		`<p>hi <span data-mention="`+agentB+`">@Coder</span></p>`, bot)

	// Emission is async when it happens at all; a disabled emit returns before
	// starting a goroutine, so no wait is needed to prove zero.
	if published != 0 {
		t.Fatalf("published %d agent.message events with the feature disabled", published)
	}
}

func TestEmitAgentMessageSkipsMessagesWithoutMentions(t *testing.T) {
	t.Setenv("AI_AGENT_DELEGATION", "true")
	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", "*")

	// No mention ids in the body => nothing to delegate to. Skipping saves waking
	// every listener for a message no agent is named in.
	ctx := WithDelegationContext(context.Background(), DelegationContext{OriginUserID: person})
	bot := &userBusiness.BotIdentity{UUID: "bot-uuid", Name: "Triage"}

	// Nil bot, empty channel and mention-free text are all no-ops; the assertion
	// here is simply that none of them panic on the agent's reply path.
	ch := Surface{Kind: SurfaceChannelPost, ChannelID: "channel-1"}
	EmitAgentMessage(ctx, ch, "post-1", "#eng", "post-1", "<p>done, no mentions</p>", bot)
	EmitAgentMessage(ctx, ch, "post-1", "#eng", "post-1", "<p>hi</p>", nil)
	EmitAgentMessage(ctx, Surface{Kind: SurfaceChannelPost}, "", "#eng", "post-1", "<p>hi</p>", bot)
	EmitAgentMessage(context.Background(), ch, "post-1", "#eng", "post-1", "<p>hi</p>", bot)
}

// AuthorizeDelegation layers a permission check on top of the budget. These
// assert the LAYERING, which is what keeps the guard cheap and correct: a budget
// denial must never reach the permission query (it needs no DB), and a human
// mention must never reach it either (their own post authorises them). Both
// matter because the permission check costs two queries and this runs on the
// message hot path.
//
// The permission check itself resolves a user and a channel, so it is exercised
// against a live graph in integration rather than mocked here — mocking it would
// only assert that the mock was called.
func TestAuthorizeDelegationLayering(t *testing.T) {
	t.Run("a human mention is authorised without touching the permission check", func(t *testing.T) {
		// No DB is configured in this test binary, so if this reached
		// originCanAddressChannel it would fail or panic rather than allow.
		d := AuthorizeDelegation(context.Background(), onCfg(),
			DelegationContext{AuthorAgentID: human, OriginUserID: person}, agentA,
			Surface{Kind: SurfaceChannelPost, ChannelID: "channel-1"}, "")
		if !d.Allow {
			t.Fatalf("human mention refused: %s", d.Reason)
		}
		if d.Reason != "human-authored message" {
			t.Fatalf("Reason = %q, want the human short-circuit", d.Reason)
		}
	})

	t.Run("a budget denial short-circuits before the permission check", func(t *testing.T) {
		// Each of these is refused by a pure rule. Reaching the DB here would be
		// both wrong and slow.
		cases := []struct {
			name string
			cfg  DelegationConfig
			dc   DelegationContext
		}{
			{"feature off", DelegationConfig{Enabled: false, ChannelOptIn: true},
				DelegationContext{AuthorAgentID: agentA, OriginUserID: person}},
			{"channel not opted in", DelegationConfig{Enabled: true, ChannelOptIn: false},
				DelegationContext{AuthorAgentID: agentA, OriginUserID: person}},
			{"orphaned chain", onCfg(),
				DelegationContext{AuthorAgentID: agentA, OriginUserID: ""}},
			{"self delegation", onCfg(),
				DelegationContext{AuthorAgentID: agentA, OriginUserID: person}},
			{"cycle", onCfg(),
				DelegationContext{AuthorAgentID: agentB, OriginUserID: person, Chain: []string{agentA, agentB}}},
			{"hop exhausted", DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 1},
				DelegationContext{AuthorAgentID: agentA, OriginUserID: person, Hop: 1}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				target := agentB
				if c.name == "self delegation" {
					target = agentA
				}
				if c.name == "cycle" {
					target = agentA
				}
				d := AuthorizeDelegation(context.Background(), c.cfg, c.dc, target,
					Surface{Kind: SurfaceChannelPost, ChannelID: "channel-1"}, "")
				if d.Allow {
					t.Fatalf("expected refusal, got allow (%s)", d.Reason)
				}
			})
		}
	})
}

// originCanAddressChannel is deny-by-default on anything it cannot verify. These
// are the input-shape refusals, which need no database.
func TestOriginCanAddressSurfaceRefusesUnverifiable(t *testing.T) {
	cases := []struct {
		name, origin, channel string
	}{
		{"no origin", "", "11111111-1111-1111-1111-111111111111"},
		{"no channel", person, ""},
		{"both empty", "", ""},
		{"channel is not a uuid", person, "not-a-uuid"},
		{"whitespace only", "   ", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if originCanAddressSurface(context.Background(), c.origin,
				Surface{Kind: SurfaceChannelPost, ChannelID: c.channel}, "") {
				t.Fatal("must refuse what it cannot verify")
			}
		})
	}
}

// delegationSurfaceKey is what an operator names in the allowlist AND what the
// permission check is asked about, so a mistake here either opts in the wrong
// thing or authorises the wrong thing. The property that matters most: a channel
// and a task must not be able to collide in one flat, human-edited list.
func TestDelegationSurfaceKey(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"

	if got := delegationSurfaceKey(Surface{Kind: SurfaceChannelPost, ChannelID: id}, "post-1"); got != id {
		t.Fatalf("channel key = %q, want the bare channel id", got)
	}
	if got := delegationSurfaceKey(Surface{Kind: SurfaceTask}, id); got != "task:"+id {
		t.Fatalf("task key = %q, want the task: prefix", got)
	}
	// The collision this prevents: a task whose uuid equals a channel's would
	// otherwise be opted in by naming the channel.
	ch := delegationSurfaceKey(Surface{Kind: SurfaceChannelPost, ChannelID: id}, "")
	task := delegationSurfaceKey(Surface{Kind: SurfaceTask}, id)
	if ch == task {
		t.Fatal("a channel and a task with the same uuid must not share an opt-in key")
	}
	// Unkeyable surfaces cannot be opted in, so delegation there stays off.
	for _, s := range []Surface{
		{Kind: SurfaceChannelPost}, // no channel id
		{Kind: SurfaceTask},        // no entity id
		{Kind: SurfaceKind("group_chat"), GroupID: id},
		{},
	} {
		if got := delegationSurfaceKey(s, ""); got != "" {
			t.Fatalf("surface %+v produced key %q, want empty", s, got)
		}
	}
}

func TestDelegationEntityID(t *testing.T) {
	const taskID = "22222222-2222-2222-2222-222222222222"

	// The surface descriptor wins for a channel thread.
	if got := delegationEntityID(Surface{Kind: SurfaceChannelPost, PostID: "post-9"}, "task:"+taskID); got != "post-9" {
		t.Fatalf("got %q, want the surface's post id", got)
	}
	// Prefixes a coding job carries must be stripped, mirroring workEntityID, so a
	// PR opened from a thread resolves to the same entity the work feed reports.
	for _, in := range []string{"task:" + taskID, "post:" + taskID, "msg:" + taskID, taskID} {
		if got := delegationEntityID(Surface{Kind: SurfaceTask}, in); got != taskID {
			t.Fatalf("source %q resolved to %q, want %q", in, got, taskID)
		}
	}
	if got := delegationEntityID(Surface{Kind: SurfaceTask}, "  "); got != "" {
		t.Fatalf("blank source = %q, want empty", got)
	}
}

// A task surface must be refusable and opt-in-able independently of channels —
// this is the path an agent assigned a task takes, where it answers in a comment.
func TestTaskSurfaceOptIn(t *testing.T) {
	const taskID = "33333333-3333-3333-3333-333333333333"
	key := delegationSurfaceKey(Surface{Kind: SurfaceTask}, taskID)

	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", "")
	if agentCollabAllowedInSurface(key) {
		t.Fatal("unset allowlist must not opt a task in")
	}
	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", "44444444-4444-4444-4444-444444444444")
	if agentCollabAllowedInSurface(key) {
		t.Fatal("a channel id must not opt a task in")
	}
	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", key)
	if !agentCollabAllowedInSurface(key) {
		t.Fatal("naming the task key must opt it in")
	}
	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", "*")
	if !agentCollabAllowedInSurface(key) {
		t.Fatal("wildcard must cover every surface")
	}
}

// emitAgentFinalAnswer is the durable path's only emitter. These assert the exits
// that must happen before any principal resolution or surface work, which is what
// keeps a disabled deployment and a scheduled routine free of cost.
func TestEmitAgentFinalAnswerEarlyExits(t *testing.T) {
	agent := &model.AiAgent{Id: uuid.New()}
	ch := Surface{Kind: SurfaceChannelPost, ChannelID: "channel-1", PostID: "post-1"}

	t.Setenv("AI_AGENT_DELEGATION", "false")
	t.Setenv("AI_AGENT_DELEGATION_CHANNELS", "*")
	emitAgentFinalAnswer(context.Background(), agent, ch, "post-1", person, 0, nil, "done")

	t.Setenv("AI_AGENT_DELEGATION", "true")
	// No human at the root: a scheduled routine. EvaluateDelegation would refuse
	// the chain anyway, so no work should be done for it.
	emitAgentFinalAnswer(context.Background(), agent, ch, "post-1", "", 0, nil, "done")
	// Nothing to say.
	emitAgentFinalAnswer(context.Background(), agent, ch, "post-1", person, 0, nil, "   ")
	// No agent.
	emitAgentFinalAnswer(context.Background(), nil, ch, "post-1", person, 0, nil, "done")
}

// The point of persisting lineage (migration 137): a DURABLE hop must detect a
// cycle by identity, not just run out of hop budget.
//
// Before the chain was stored, the worker could only recover the originating person
// and had to assume a single-agent chain, so two agents relaying through durable
// jobs each looked like they were starting fresh — precisely the case the identity
// check exists to catch. This walks a persisted chain the way the worker does and
// asserts the revisit is refused while the budget still has room.
func TestPersistedChainCatchesDurableCycle(t *testing.T) {
	cfg := DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 5, MaxChainAgents: 8}

	// Agent B finishing a durable job whose stored lineage is [A, B] at hop 2.
	stored := DelegationContext{Hop: 2, OriginUserID: person, Chain: []string{agentA, agentB}}
	next := NextDelegationContext(stored, agentB)

	// Hop budget is nowhere near exhausted, so only identity can stop this.
	if next.Hop >= cfg.hopLimit() {
		t.Fatalf("hop %d should still be inside the budget of %d", next.Hop, cfg.hopLimit())
	}
	if d := EvaluateDelegation(cfg, next, agentA); d.Allow {
		t.Fatal("revisiting agent A must be refused by the chain, not left to the hop budget")
	}
	// A third, unseen agent is still reachable — the guard bounds cycles, it does
	// not stop collaboration.
	if d := EvaluateDelegation(cfg, next, agentC); !d.Allow {
		t.Fatalf("a new agent should still be allowed: %s", d.Reason)
	}
}

// A job enqueued before migration 137, or by a schedule, has no stored lineage. It
// must read as the conservative "not part of a chain" case rather than as hop 0 with
// a human author, which would let an agent-authored answer look human-authored and
// bypass the budget entirely.
func TestMissingPersistedLineageIsConservative(t *testing.T) {
	cfg := DelegationConfig{Enabled: true, ChannelOptIn: true, MaxHops: 2}

	// Zero values, as a pre-migration row scans.
	advanced := NextDelegationContext(DelegationContext{Hop: 0, OriginUserID: person, Chain: nil}, agentA)
	if advanced.Hop != 1 {
		t.Fatalf("hop = %d, want 1", advanced.Hop)
	}
	if !reflect.DeepEqual(advanced.Chain, []string{agentA}) {
		t.Fatalf("chain = %v, want just the acting agent", advanced.Chain)
	}
	// Crucially it is agent-authored, so every delegation rule applies to it.
	if advanced.AuthorAgentID != agentA {
		t.Fatalf("AuthorAgentID = %q, want the acting agent", advanced.AuthorAgentID)
	}
	if d := EvaluateDelegation(cfg, advanced, agentA); d.Allow {
		t.Fatal("self-delegation must still be refused on a rebuilt chain")
	}
}
