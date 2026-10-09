//go:build integration
// +build integration

package integration_test

// Guest links against real Postgres, Dgraph and Redis.
//
// What a guest writes reaches the team, as a member's words would:
//   - a message in a shared channel tells its members, by each one's setting
//     for the channel (everything, mentions only), and nobody outside it;
//   - a reply in a thread tells the post's author and those who replied
//     before, and not the rest of the channel;
//   - a comment on a shared doc tells whoever made it and its editors, not
//     those who can only read it.
//
// Once per channel, thread or doc per few minutes, and a comment nobody could
// be told of (its doc unreadable just then) doesn't spend those minutes.
//
// Pushes are watched at the one place every path sends them, so this checks
// whom each reaches without a Firebase project.
//
// A shared doc's page is titled with the doc's name. A channel archived since
// its link was made takes no message or reply from it, and tells nobody.
//
// And a guest's page is told "This link is no longer available" (or that a
// message or task isn't there) only when that is true: when the server can't
// answer (Dgraph or Postgres down, a restart), every guest read, the channel,
// a thread, a project and a task, gets a 503, which the page retries, instead
// of the "not available" that made a client's page give up on a link that
// still worked. The workspace's settings come from Postgres too, the way they
// do in production.
//
// One test, one environment: the "Guests" principal is made once and kept for
// the process (business/User), so a second environment would be handed the
// first one's principal, and a guest's message would be written by a user its
// database has never seen.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGuestLinks -v

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/tests/integration"
)

type sentPush struct {
	data   map[string]string
	tokens []string
}

func TestGuestLinks(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()
	// pushOf waits for the push of a kind and says which devices it went to.
	pushOf := func(t *testing.T, kind string) sentPush {
		t.Helper()
		var found sentPush
		waitFor(t, "a "+kind+" push", func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind {
					found = p
					return true
				}
			}
			return false
		})
		slices.Sort(found.tokens)
		return found
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// ada and cy hear of everything in #launch, bo only of mentions, and dee
	// isn't in it. olu made the brief, ed edits it and rea only reads it.
	people := map[string]uuid.UUID{}
	for _, name := range []string{"ada", "bo", "cy", "dee", "olu", "ed", "rea"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username) VALUES ($1, $2, $3)`, id, name+"@acme.test", name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "tok-"+name)
	}
	channel, doc, archived := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel)
	exec(`INSERT INTO channels (id, ch_name, ch_private, deleted_at) VALUES ($1, 'old-launch', false, NOW())`, archived)
	exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, 'all')`, people["cy"], archived)
	for name, setting := range map[string]string{"ada": "all", "bo": "mention", "cy": "all"} {
		exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, $3)`,
			people[name], channel, setting)
	}
	zero := "0001-01-01T00:00:00Z"
	user := func(name string) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": people[name].String(), "user_name": name}
	}
	post, project, task, oldPost, notes := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uids := dg.Mutate(t, []map[string]any{
		user("ada"), user("bo"), user("cy"), user("dee"), user("olu"), user("ed"), user("rea"),
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:ada"}, {"uid": "_:bo"}, {"uid": "_:cy"}}},
		// bo asked something; ada answered.
		{"uid": "_:post", "dgraph.type": "Post", "post_uuid": post.String(), "post_text": "<p>Launch on Friday?</p>",
			"post_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339), "post_deleted_at": zero,
			"post_by": map[string]any{"uid": "_:bo"}, "post_channel": map[string]any{"uid": "_:ch"},
			"post_comments": []map[string]any{{"uid": "_:answer", "dgraph.type": "Comment", "comment_uuid": uuid.NewString(),
				"comment_text": "<p>Yes</p>", "comment_deleted_at": zero, "comment_by": map[string]any{"uid": "_:ada"}}}},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc.String(), "doc_title": "Launch brief", "doc_private": true,
			"doc_deleted_at": zero, "doc_created_by": map[string]any{"uid": "_:olu"},
			"doc_editing_users": []map[string]any{{"uid": "_:ed"}}, "doc_reading_users": []map[string]any{{"uid": "_:rea"}}},
		{"uid": "_:notes", "dgraph.type": "Doc", "doc_uuid": notes.String(), "doc_title": "Release notes", "doc_private": true,
			"doc_deleted_at": zero, "doc_created_by": map[string]any{"uid": "_:olu"}},
		{"uid": "_:old", "dgraph.type": "Channel", "ch_uuid": archived.String(), "ch_name": "old-launch", "ch_private": false,
			"ch_deleted_at": time.Now().Add(-time.Hour).Format(time.RFC3339), "ch_members": []map[string]any{{"uid": "_:cy"}},
			"ch_posts": []map[string]any{{"uid": "_:oldpost", "dgraph.type": "Post", "post_uuid": oldPost.String(),
				"post_text": "<p>Shipped</p>", "post_deleted_at": zero, "post_by": map[string]any{"uid": "_:cy"},
				"post_channel": map[string]any{"uid": "_:old"}}}},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Website",
			"project_deleted_at": zero, "project_tasks": []map[string]any{{"uid": "_:task"}}},
		{"uid": "_:task", "dgraph.type": "Task", "task_uuid": task.String(), "task_name": "Home page", "task_status": "todo",
			"task_deleted_at": zero, "task_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
			"task_project": map[string]any{"uid": "_:project"}},
	})

	link := func(t *testing.T, kind, id, capability string) (*guestBusiness.ResourceGrant, *guestModel.GuestGrant) {
		t.Helper()
		g, err := guestBusiness.CreateResourceGrant(ctx, people["olu"], kind, id, capability, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := guestModel.GetByID(ctx, g.GrantID)
		if err != nil || grant == nil {
			t.Fatalf("grant: %v", err)
		}
		return g, grant
	}
	channelRaw, channelLink := link(t, guestModel.ResourceChannel, channel.String(), guestModel.CapabilityPost)
	_, docLink := link(t, guestModel.ResourceDoc, doc.String(), guestModel.CapabilityComment)
	_, notesLink := link(t, guestModel.ResourceDoc, notes.String(), guestModel.CapabilityComment)

	t.Run("a message tells the channel's members by their setting", func(t *testing.T) {
		if err := guestBusiness.PostAsGuest(ctx, channelLink, "Priya", "Is the launch still on?", ""); err != nil {
			t.Fatal(err)
		}
		p := pushOf(t, firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL)
		if !slices.Equal(p.tokens, []string{"tok-ada", "tok-cy"}) {
			t.Errorf("went to %v, want ada and cy (bo hears only of mentions, dee isn't in it)", p.tokens)
		}
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "#launch - Priya (guest)" ||
			p.data[firebaseInit.FIREBASE_PUSH_DATA_BODY] != "Is the launch still on?" ||
			p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != channel.String() {
			t.Errorf("says %v", p.data)
		}
	})

	t.Run("a reply tells the thread's people", func(t *testing.T) {
		if err := guestBusiness.PostAsGuest(ctx, channelLink, "Priya", "Great, thanks", post.String()); err != nil {
			t.Fatal(err)
		}
		p := pushOf(t, firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT)
		if !slices.Equal(p.tokens, []string{"tok-ada", "tok-bo"}) {
			t.Errorf("went to %v, want bo (who asked) and ada (who answered), not cy", p.tokens)
		}
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Comment - Priya (guest)" ||
			p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != post.String() {
			t.Errorf("says %v", p.data)
		}
	})

	t.Run("a doc comment tells its maker and editors", func(t *testing.T) {
		if _, err := guestBusiness.CreateGuestDocComment(ctx, docLink, "Priya", "Page 2 has the old price."); err != nil {
			t.Fatal(err)
		}
		p := pushOf(t, firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT)
		if !slices.Equal(p.tokens, []string{"tok-ed", "tok-olu"}) {
			t.Errorf("went to %v, want olu (made it) and ed (edits it), not rea", p.tokens)
		}
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Comment - Priya (guest)" ||
			p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != doc.String() {
			t.Errorf("says %v", p.data)
		}
	})

	t.Run("a burst of a guest's words wakes the team once", func(t *testing.T) {
		count := func(kind, field, id string) int {
			mu.Lock()
			defer mu.Unlock()
			n := 0
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind && p.data[field] == id {
					n++
				}
			}
			return n
		}
		if err := guestBusiness.PostAsGuest(ctx, channelLink, "Priya", "And one more thing", ""); err != nil {
			t.Fatal(err)
		}
		if err := guestBusiness.PostAsGuest(ctx, channelLink, "Priya", "Sorry, two", post.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := guestBusiness.CreateGuestDocComment(ctx, docLink, "Priya", "And page 3."); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second) // pushes would be on their way by now
		for what, n := range map[string]int{
			"channel messages": count(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL, firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID, channel.String()),
			"thread replies":   count(firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT, firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID, post.String()),
			"doc comments":     count(firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT, firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID, doc.String()),
		} {
			if n != 1 {
				t.Errorf("%d pushes for two %s within minutes, want one", n, what)
			}
		}
	})

	t.Run("a comment nobody could be told of leaves the next to tell them", func(t *testing.T) {
		aboutNotes := func() int {
			mu.Lock()
			defer mu.Unlock()
			n := 0
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT &&
					p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == notes.String() {
					n++
				}
			}
			return n
		}
		// The doc can't be read as the comment lands (deleted for a moment
		// here; a Dgraph blip in life): nobody is told.
		dg.Mutate(t, map[string]any{"uid": uids["notes"], "doc_deleted_at": time.Now().Format(time.RFC3339)})
		if _, err := guestBusiness.CreateGuestDocComment(ctx, notesLink, "Priya", "Is this the latest?"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond) // its notice would be on its way by now
		if n := aboutNotes(); n != 0 {
			t.Fatalf("%d pushes about a doc that couldn't be read", n)
		}
		// The next comment, a moment later, tells them.
		dg.Mutate(t, map[string]any{"uid": uids["notes"], "doc_deleted_at": zero})
		if _, err := guestBusiness.CreateGuestDocComment(ctx, notesLink, "Priya", "Found it, thanks."); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a push about the doc", func() bool { return aboutNotes() == 1 })
	})

	t.Run("a shared doc's page is titled with its name", func(t *testing.T) {
		if got := guestBusiness.SharedTitle(ctx, docLink); got != "Launch brief" {
			t.Errorf("title %q", got)
		}
	})

	t.Run("an archived channel takes nothing and tells nobody", func(t *testing.T) {
		_, oldLink := link(t, guestModel.ResourceChannel, archived.String(), guestModel.CapabilityPost)
		for what, replyTo := range map[string]string{"a message": "", "a reply": oldPost.String()} {
			if err := guestBusiness.PostAsGuest(ctx, oldLink, "Priya", "Are you still there?", replyTo); !errors.Is(err, guestBusiness.ErrNotFound) {
				t.Errorf("%s in an archived channel: %v, want ErrNotFound", what, err)
			}
		}
		var written int
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, archived).Scan(&written); err != nil || written != 0 {
			t.Errorf("%d posts written into the archived channel (%v)", written, err)
		}
		time.Sleep(500 * time.Millisecond) // a push would be on its way by now
		mu.Lock()
		defer mu.Unlock()
		for _, p := range pushes {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == archived.String() || p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == oldPost.String() {
				t.Errorf("the archived channel's members were told: %v", p.data)
			}
		}
	})

	// Last: it stops Dgraph and Postgres.
	t.Run("a link is only gone when it is", func(t *testing.T) {
		revoked, _ := link(t, guestModel.ResourceChannel, channel.String(), guestModel.CapabilityView)
		if err := guestModel.Revoke(ctx, revoked.GrantID); err != nil {
			t.Fatal(err)
		}
		projectRaw, _ := link(t, guestModel.ResourceProject, project.String(), guestModel.CapabilityComment)
		boardRaw, _ := link(t, guestModel.ResourceBoard, uuid.NewString(), guestModel.CapabilityView)
		open := func(handler http.HandlerFunc, params ...string) (int, string) {
			t.Helper()
			rc := chi.NewRouteContext()
			for i := 0; i+1 < len(params); i += 2 {
				rc.URLParams.Add(params[i], params[i+1])
			}
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
			return rec.Code, rec.Body.String()
		}
		reads := map[string]func() (int, string){
			"the channel": func() (int, string) { return open(guestController.GuestChannel, "token", channelRaw.Token) },
			"a thread": func() (int, string) {
				return open(guestController.GuestChannelThread, "token", channelRaw.Token, "post_id", post.String())
			},
			"the project": func() (int, string) { return open(guestController.GuestProject, "token", projectRaw.Token) },
			"a project task": func() (int, string) {
				return open(guestController.GuestProjectTask, "token", projectRaw.Token, "task_id", task.String())
			},
		}
		expect := func(want int, why string) {
			t.Helper()
			for name, read := range reads {
				if code, body := read(); code != want {
					t.Errorf("%s %s: %d %s, want %d", name, why, code, body, want)
				}
			}
		}

		expect(http.StatusOK, "on a live link")
		gone := map[string]func() (int, string){
			"a revoked link": func() (int, string) { return open(guestController.GuestChannel, "token", revoked.Token) },
			"no link at all": func() (int, string) { return open(guestController.GuestChannel, "token", "not-a-link") },
			"a thread that's gone": func() (int, string) {
				return open(guestController.GuestChannelThread, "token", channelRaw.Token, "post_id", uuid.NewString())
			},
			"a task that isn't one": func() (int, string) {
				return open(guestController.GuestProjectTask, "token", projectRaw.Token, "task_id", uuid.NewString())
			},
			"a board image that isn't one": func() (int, string) {
				return open(guestController.GuestBoardAttachment, "token", boardRaw.Token, "obj_uuid", uuid.NewString())
			},
		}
		for name, read := range gone {
			if code, body := read(); code != http.StatusNotFound {
				t.Errorf("%s: %d %s, want 404", name, code, body)
			}
		}

		// A board image whose record can't be read just then (here, its table
		// away for a moment): retried, not given up on.
		image := func() (int, string) {
			return open(guestController.GuestBoardAttachment, "token", boardRaw.Token, "obj_uuid", uuid.NewString())
		}
		exec(`ALTER TABLE attachments RENAME TO attachments_away`)
		if code, body := image(); code != http.StatusServiceUnavailable {
			t.Errorf("a board image while its record can't be read: %d %s, want 503", code, body)
		}
		exec(`ALTER TABLE attachments_away RENAME TO attachments`)

		// Dgraph stops answering: the links still work, so the pages should retry.
		dg.Close()
		expect(http.StatusServiceUnavailable, "while Dgraph doesn't answer")

		// Postgres stops answering: nothing is known about the links.
		if err := postgresInit.DBConn.SqlDB.Close(); err != nil {
			t.Fatal(err)
		}
		expect(http.StatusServiceUnavailable, "while Postgres doesn't answer")
		if code, body := image(); code != http.StatusServiceUnavailable {
			t.Errorf("a board image while Postgres doesn't answer: %d %s, want 503", code, body)
		}
	})
}
