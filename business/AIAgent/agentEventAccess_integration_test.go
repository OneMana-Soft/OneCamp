//go:build integration

package business

import (
	"context"
	"errors"
	"testing"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// What reaches an agent unasked, with the real lookups against Postgres and
// Dgraph: an event or an ambient message only from where its sponsor can
// see, and an agent can't be saved pointing anywhere else.
//
// Run: go test -tags=integration ./business/AIAgent/ -run TestAnAgentHearsOnlyWhatItsSponsorCanSee -v
func TestAnAgentHearsOnlyWhatItsSponsorCanSee(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	sponsor := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, sponsor, sponsor.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	mine, finance, general := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ours, payroll := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sponsor", "user_uuid": sponsor.String(), "user_name": "sponsor"},
		{"uid": "_:mine", "ch_uuid": mine, "ch_name": "mine", "ch_private": true,
			"ch_members": []map[string]any{{"uid": "_:sponsor"}}},
		{"uid": "_:finance", "ch_uuid": finance, "ch_name": "finance", "ch_private": true},
		{"uid": "_:general", "ch_uuid": general, "ch_name": "general", "ch_private": false},
		{"uid": "_:ours", "project_uuid": ours, "project_name": "ours",
			"project_members": []map[string]any{{"uid": "_:sponsor"}}},
		{"uid": "_:payroll", "project_uuid": payroll, "project_name": "payroll"},
	})

	s := sponsor.String()
	for _, c := range []struct {
		name, event string
		data        map[string]interface{}
		want        bool
	}{
		{"their private channel", "post.created", map[string]interface{}{"channel_id": mine}, true},
		{"a public channel", "post.created", map[string]interface{}{"channel_id": general}, true},
		{"a private channel they aren't in", "post.created", map[string]interface{}{"channel_id": finance}, false},
		{"their project", "task.created", map[string]interface{}{"project_id": ours}, true},
		{"a project they aren't in", "task.status_changed", map[string]interface{}{"project_id": payroll}, false},
	} {
		if got := reach.sees(ctx, s, c.event, c.data); got != c.want {
			t.Errorf("%s: sees=%v, want %v", c.name, got, c.want)
		}
	}

	for _, c := range []struct {
		channel string
		want    bool
	}{{general, true}, {mine, false}, {finance, false}} {
		if got, err := channelBusiness.IsPublic(ctx, s, c.channel); err != nil || got != c.want {
			t.Errorf("IsPublic(%s) = %v, %v; want %v", c.channel, got, err, c.want)
		}
	}

	a := &model.AiAgent{Id: uuid.New(), Name: "Helper", Ambient: true, CreatedBy: sponsor}
	question := "Does anyone know when the vendor contract renews?"
	if !ambientConsiders(ctx, a, mine, "someone", question, nil) || ambientConsiders(ctx, a, finance, "someone", question, nil) {
		t.Error("an ambient agent considers its sponsor's channels, and never a private one they aren't in")
	}

	if err := reach.checkReach(ctx, s, model.TriggerMention, `{}`, `{"channel_ids":["`+mine+`","`+general+`"],"project_ids":["`+ours+`"]}`); err != nil {
		t.Errorf("scoped to where its sponsor can go: %v", err)
	}
	if err := reach.checkReach(ctx, s, model.TriggerMention, `{}`, `{"channel_ids":["`+finance+`"]}`); !errors.Is(err, errScopeChannel) {
		t.Errorf("scoped to a private channel its sponsor isn't in: %v", err)
	}
	if err := reach.checkReach(ctx, s, model.TriggerEvent, `{"event":"task.status_changed","project_id":"`+payroll+`"}`, ``); !errors.Is(err, errScopeProject) {
		t.Errorf("watching moves in a project its sponsor isn't in: %v", err)
	}

	// Saving goes through the same check, whoever saves: the agent acts as
	// the person it works for.
	in := func(channel string) AgentInput {
		return AgentInput{
			Name:          "Helper " + channel[:4],
			Instructions:  "Answer questions.",
			TriggerType:   model.TriggerMention,
			TriggerConfig: map[string]interface{}{},
			Scope:         map[string]interface{}{"channel_ids": []interface{}{channel}},
			IsActive:      true,
			MaxSteps:      4,
		}
	}
	if _, err := CreateAgent(ctx, in(finance), sponsor); !errors.Is(err, errScopeChannel) {
		t.Fatalf("creating one scoped to a channel its sponsor can't read: %v", err)
	}
	created, err := CreateAgent(ctx, in(mine), sponsor)
	if err != nil {
		t.Fatalf("creating one scoped to its sponsor's channel: %v", err)
	}
	admin := Actor{UserID: uuid.New(), IsAdmin: true}
	if _, err := UpdateAgent(ctx, created.Id, in(finance), admin); !errors.Is(err, errScopeChannel) {
		t.Fatalf("an admin pointing it at a channel its sponsor can't read: %v", err)
	}
	if _, err := UpdateAgent(ctx, created.Id, in(general), admin); err != nil {
		t.Fatalf("an admin pointing it at a public channel: %v", err)
	}
}
