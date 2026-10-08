//go:build integration

package business

// The bot list against Postgres 12 with every migration: each bot by its kind,
// and no person in it.
// Run: go test -tags=integration ./business/User/ -run TestBotKinds -v

import (
	"context"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestBotKinds(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	checkin, err := domain.EnsureBotUser(ctx, domain.CheckInBotEmail, domain.CheckInBotUsername, "Check-in")
	if err != nil {
		t.Fatal(err)
	}
	agentEmail := domain.AgentBotPrefix + uuid.NewString() + domain.BotEmailDomain
	agent, err := domain.EnsureBotUser(ctx, agentEmail, "agent-bot-x", "Release Captain")
	if err != nil {
		t.Fatal(err)
	}
	person := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya@example.com', NOW(), NOW())`, person); err != nil {
		t.Fatal(err)
	}

	kinds, err := BotKinds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kinds[checkin.String()] != string(domain.BotKindCheckIn) || kinds[agent.String()] != string(domain.BotKindAgent) {
		t.Fatalf("each bot by its kind: %v", kinds)
	}
	if _, ok := kinds[person.String()]; ok {
		t.Fatalf("a person is in the bot list: %v", kinds)
	}
}
