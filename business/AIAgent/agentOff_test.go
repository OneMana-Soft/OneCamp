package business

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The kill switch is read afresh each step: a paused or deleted agent, or one
// whose sponsor has left, stops; a lookup that fails does not, since the next
// step reads it again.
func TestAgentOffReason(t *testing.T) {
	sponsor := uuid.New()
	live := &model.AiAgent{Id: uuid.New(), IsActive: true, CreatedBy: sponsor}
	paused := &model.AiAgent{Id: uuid.New(), IsActive: false, CreatedBy: sponsor}
	member := &userModels.User{Id: sponsor}
	const left = "the person this AI teammate works for has left the workspace"

	cases := []struct {
		name       string
		agent      *model.AiAgent
		agentErr   error
		sponsor    *userModels.User
		sponsorErr error
		want       string
	}{
		{"on, sponsor here", live, nil, member, nil, ""},
		{"paused", paused, nil, member, nil, "this AI teammate is paused"},
		{"deleted", nil, nil, member, nil, "this AI teammate was deleted"},
		{"sponsor left", live, nil, nil, sql.ErrNoRows, left},
		{"sponsor row empty", live, nil, &userModels.User{}, nil, left},
		{"agent lookup fails", nil, errors.New("db down"), member, nil, ""},
		{"sponsor lookup fails", live, nil, nil, errors.New("db down"), ""},
	}
	readAgent, readSponsor := offReadAgent, offReadSponsor
	t.Cleanup(func() { offReadAgent, offReadSponsor = readAgent, readSponsor })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			offReadAgent = func(context.Context, uuid.UUID) (*model.AiAgent, error) { return c.agent, c.agentErr }
			offReadSponsor = func(_ context.Context, id uuid.UUID) (*userModels.User, error) {
				if id != sponsor {
					t.Fatalf("asked about %s, not the agent's sponsor", id)
				}
				return c.sponsor, c.sponsorErr
			}
			if got := agentOffReason(context.Background(), uuid.New()); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
