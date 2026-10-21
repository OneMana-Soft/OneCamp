//go:build integration

package domain_test

import (
	"context"
	"fmt"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The profile every signed-in request reads (on a cache miss) carries no posts.
//
// It used to name user_posts with no fields under it. A uid edge selected that
// way returns nothing in DQL (verified on v25.5.0: the edge is left out of the
// answer), so the line was dead text that read like a read of every post; it
// went in DG4 with four like it. This holds the answer to what it always was.
func TestTheProfileDoesNotCarryEveryPostThePersonWrote(t *testing.T) {
	ctx := context.Background()
	// The profile's task counts leave out restricted tasks, read from Postgres (models/taskread).
	integration.ConnectServerPostgres(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)

	me := uuid.New()
	nodes := []map[string]any{{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "prolific",
		"user_deleted_at": "0001-01-01T00:00:00Z"}}
	var posts []map[string]any
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("_:p%d", i)
		nodes = append(nodes, map[string]any{"uid": id, "dgraph.type": "Post", "post_uuid": uuid.NewString(),
			"post_text": "hello", "post_by": map[string]any{"uid": "_:me"}})
		posts = append(posts, map[string]any{"uid": id})
	}
	nodes[0]["user_posts"] = posts
	dg.Mutate(t, nodes)

	profile, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, me.String())
	if err != nil || profile == nil {
		t.Fatalf("profile: %v, %v", profile, err)
	}
	if profile.Uuid != me.String() {
		t.Fatalf("read %q, want %q", profile.Uuid, me)
	}
	if n := len(profile.Posts); n != 0 {
		t.Errorf("the profile carries %d post uid(s); it must carry none", n)
	}
}
