//go:build integration
// +build integration

package integration_test

// Who sees a comment on a doc or a board in their activity feed, against a
// real Postgres and Dgraph.
//
// A comment's mentions are stored for everyone it names, whether or not they
// may read the doc or board it is on, and the feed held such a comment to its
// doc's or board's deletion alone. Someone named on a private doc they
// couldn't read, or taken off one later, found the comment's text and the
// doc's current title in their feed and in the mentions tab, and a deleted
// doc's comments kept their text there. Now an item about a doc or a board is
// kept only if the doc or the board says the reader may read it now. The graph
// is seeded as the comment paths store it, mentions of people who can't read
// included, since that is what installs already hold.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheFeedShowsCommentsOnlyOnWhatTheReaderMayRead -v

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	_ "github.com/akashc777/OneCamp/business/Board" // hands the feed the board read rule
	_ "github.com/akashc777/OneCamp/business/Doc"   // hands the feed the doc read rule
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheFeedShowsCommentsOnlyOnWhatTheReaderMayRead(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	people := map[string]uuid.UUID{"owner": uuid.New(), "reader": uuid.New(), "outsider": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	const live = "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	// The owner's private doc and board, shared with the reader, and a public
	// doc and board.
	nodes = append(nodes,
		map[string]any{"uid": "_:salaries", "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_title": "Salaries",
			"doc_private": true, "doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:owner"},
			"doc_reading_users": []map[string]any{{"uid": "_:reader"}}},
		map[string]any{"uid": "_:handbook", "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_title": "Handbook",
			"doc_private": false, "doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:owner"}},
		map[string]any{"uid": "_:layoffs", "dgraph.type": "Board", "board_uuid": uuid.NewString(), "board_title": "Layoffs",
			"board_private": true, "board_deleted_at": live, "board_created_by": map[string]any{"uid": "_:owner"},
			"board_reading_users": []map[string]any{{"uid": "_:reader"}}},
		map[string]any{"uid": "_:retro", "dgraph.type": "Board", "board_uuid": uuid.NewString(), "board_title": "Retro",
			"board_private": false, "board_deleted_at": live, "board_created_by": map[string]any{"uid": "_:owner"}},
	)
	// A comment by the owner on each, naming the reader and the outsider, as
	// CreateDocComment and SyncBoardComment store one.
	texts := map[string]string{"salaries": "salary bands for next year", "handbook": "the onboarding section",
		"layoffs": "the shortlist draft", "retro": "the action items"}
	ago := 5 * time.Minute
	for parent, text := range texts {
		edge := "comment_doc"
		if parent == "layoffs" || parent == "retro" {
			edge = "comment_board"
		}
		commentUUID := uuid.NewString()
		at := now.Add(-ago).Format(time.RFC3339Nano)
		ago -= time.Minute
		nodes = append(nodes, map[string]any{"uid": "_:c-" + parent, "dgraph.type": "Comment", "comment_uuid": commentUUID,
			"comment_text": text, edge: map[string]any{"uid": "_:" + parent},
			"comment_by": map[string]any{"uid": "_:owner"}, "comment_on_content_added_by": map[string]any{"uid": "_:owner"},
			"comment_created_at": at, "comment_deleted_at": live,
			"comment_mentions": []map[string]any{{"uid": "_:m-" + parent, "dgraph.type": "Mention",
				"mention_users":        []map[string]any{{"uid": "_:reader"}, {"uid": "_:outsider"}},
				"mention_comment":      map[string]any{"uid": "_:c-" + parent},
				"mention_comment_uuid": commentUUID, "mention_created_at": at}}})
	}
	uids := dg.Mutate(t, nodes)

	// seen: the comments (by parent) whose mentions the person finds in the
	// feed, and in the mentions tab, which must agree.
	seen := func(t *testing.T, who string) string {
		t.Helper()
		byText := map[string]string{}
		for parent, text := range texts {
			byText[text] = parent
		}
		var feed, tab []string
		p, err := activityBusiness.GetUnifiedActivity(ctx, people[who], uids[who], time.Now().Add(time.Minute), 50)
		if err != nil {
			t.Fatalf("%s's feed: %v", who, err)
		}
		for _, it := range p.Activities {
			if it.Mention != nil && it.Mention.Comment != nil {
				feed = append(feed, byText[it.Mention.Comment.Text])
			}
		}
		m, err := activityBusiness.GetMentionsByUserId(ctx, uids[who], people[who].String(), 0, 50)
		if err != nil {
			t.Fatalf("%s's mentions tab: %v", who, err)
		}
		for _, it := range m.Mentions {
			if it.Comment != nil {
				tab = append(tab, byText[it.Comment.Text])
			}
		}
		sort.Strings(feed)
		sort.Strings(tab)
		if strings.Join(feed, ",") != strings.Join(tab, ",") {
			t.Errorf("%s's feed shows %v and the mentions tab %v", who, feed, tab)
		}
		return strings.Join(feed, ",")
	}
	expect := func(t *testing.T, who, want string) {
		t.Helper()
		if got := seen(t, who); got != want {
			t.Errorf("%s sees comments on %q, want %q", who, got, want)
		}
	}

	t.Run("each sees the comments on what they may read", func(t *testing.T) {
		expect(t, "reader", "handbook,layoffs,retro,salaries")
		expect(t, "outsider", "handbook,retro")
	})

	t.Run("someone taken off a doc or a board stops seeing its comments", func(t *testing.T) {
		if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{CommitNow: true, DelNquads: []byte(fmt.Sprintf(
			"<%s> <doc_reading_users> <%s> .\n<%s> <board_reading_users> <%s> .",
			uids["salaries"], uids["reader"], uids["layoffs"], uids["reader"]))}); err != nil {
			t.Fatal(err)
		}
		expect(t, "reader", "handbook,retro")
	})

	t.Run("a deleted doc's or board's comments leave the feed", func(t *testing.T) {
		deleted := time.Now().UTC().Format(time.RFC3339Nano)
		dg.Mutate(t, []map[string]any{
			{"uid": uids["handbook"], "doc_deleted_at": deleted},
			{"uid": uids["retro"], "board_deleted_at": deleted},
		})
		expect(t, "reader", "")
		expect(t, "outsider", "")
	})
}
