//go:build integration

package integration_test

// PROJ-BUG-8, the reads beside the boards: a project's, a team's and a
// channel's member lists, a client link's view of a task's comments and a
// person's recent emoji statuses were read sorted with no first, which Dgraph stops
// at 1,000 without saying so. They are read unsorted now and sorted in Go.
// This seeds 1,100 of each and checks that every one comes back, in the order
// Dgraph's sort gave.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	teamDomain "github.com/akashc777/OneCamp/domain/Team"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestMemberListsCommentsAndStatusesPastAThousand(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	const n = 1100
	now := time.Now().UTC().Truncate(time.Second)
	// Seeded out of order, so a list comes back in order only if something sorts it.
	scrambled := func(i int) int { return (i * 397) % n }

	var nodes []map[string]any
	everyone := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, map[string]any{"uid": fmt.Sprintf("_:p%d", i), "dgraph.type": "User",
			"user_uuid": uuid.NewString(), "user_name": fmt.Sprintf("person %04d", scrambled(i))})
		everyone[i] = map[string]any{"uid": fmt.Sprintf("_:p%d", i)}
	}
	var comments, statuses []map[string]any
	for i := 0; i < n; i++ {
		comments = append(comments, map[string]any{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(),
			"comment_text": fmt.Sprintf("comment %d", i), "comment_created_at": now.Add(-time.Duration(scrambled(i)) * time.Minute).Format(time.RFC3339)})
		status := map[string]any{"status_user_emoji_id": fmt.Sprintf("e%d", i), "status_user_emoji_desc": "away",
			"status_user_emoji_expiry_at": now.Add(time.Duration(1+i) * time.Minute).Format(time.RFC3339)}
		// One in eleven was set before set times were kept: those come last.
		if i%11 != 0 {
			status["status_user_emoji_set_at"] = now.Add(-time.Duration(scrambled(i)) * time.Minute).Format(time.RFC3339)
		}
		statuses = append(statuses, status)
	}
	project, team, channel, task := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"dgraph.type": "Project", "project_uuid": project, "project_name": "Everyone", "project_members": everyone, "project_admins": everyone},
		map[string]any{"dgraph.type": "Team", "team_uuid": team, "team_name": "Everyone", "team_members": everyone, "team_admins": everyone},
		map[string]any{"dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "everyone", "ch_members": everyone, "ch_moderators": everyone},
		map[string]any{"dgraph.type": "Task", "task_uuid": task, "task_name": "Talked about a lot", "task_comments": comments},
	)
	nodes[0]["user_emoji_statuses"] = statuses
	uids := dg.Mutate(t, nodes)
	someone := uids["p0"]

	byName := func(what string, people []*dgraphStruct.DgraphUser) {
		t.Helper()
		if len(people) != n {
			t.Fatalf("%s: %d people, want all %d", what, len(people), n)
		}
		if !sort.SliceIsSorted(people, func(i, j int) bool { return people[i].UserName < people[j].UserName }) {
			t.Fatalf("%s: not in name order", what)
		}
	}
	p, err := projectDomain.GetDgraphProjectMemberInfo(ctx, project, someone)
	if err != nil {
		t.Fatal(err)
	}
	byName("project members", p.Members)
	byName("project admins", p.Admins)
	tm, err := teamDomain.GetDgraphTeamMemberListByTeamUUID(ctx, team, someone)
	if err != nil {
		t.Fatal(err)
	}
	byName("team members", tm.Members)
	byName("team admins", tm.Admins)
	ch, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channel, someone)
	if err != nil {
		t.Fatal(err)
	}
	byName("channel members", ch.Members)
	byName("channel moderators", ch.Moderators)

	// A client link shows a task's comments oldest first.
	g, err := taskDomain.GetDgraphTaskForGuest(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Comments) != n {
		t.Fatalf("a client link shows %d comments, want all %d", len(g.Comments), n)
	}
	for i := 1; i < n; i++ {
		if g.Comments[i].CreatedAt.Before(*g.Comments[i-1].CreatedAt) {
			t.Fatalf("comment %d is older than the one before it", i)
		}
	}

	// A person's statuses come the one set last first, those with no set time
	// after them all, as emojiStatusOrder sorts them.
	u, err := userDomain.GetAllUserEmojiStatusList(ctx, nodes[0]["user_uuid"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.StatusEmoji) != n {
		t.Fatalf("%d emoji statuses, want all %d", len(u.StatusEmoji), n)
	}
	for i := 1; i < n; i++ {
		prev, cur := u.StatusEmoji[i-1].SetAt, u.StatusEmoji[i].SetAt
		switch {
		case prev == nil && cur != nil:
			t.Fatalf("status %d has a set time and comes after one without", i)
		case prev != nil && cur != nil && cur.After(*prev):
			t.Fatalf("status %d was set later than the one before it", i)
		}
	}
	if u.StatusEmoji[n-1].SetAt != nil {
		t.Fatalf("the statuses with no set time are not last")
	}
}
