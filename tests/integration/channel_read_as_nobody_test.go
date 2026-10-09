//go:build integration
// +build integration

package integration_test

// A Slack import's workers read each channel as nobody in particular, and the
// read refused an empty viewer ("ID can't be empty"), so every imported
// message failed and a Slack import brought channels without their history.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAChannelCanBeReadAsNobodyInParticular -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAChannelCanBeReadAsNobodyInParticular(t *testing.T) {
	dg := integration.SetupDgraph(t)
	channel := uuid.New()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "ada"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": []map[string]any{{"uid": "_:ada"}}},
	})
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(context.Background(), channel, "")
	if err != nil || ch == nil || ch.Uid == "" || ch.Name != "launch" {
		t.Fatalf("read as nobody: %+v %v", ch, err)
	}
}
