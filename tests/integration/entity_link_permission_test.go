//go:build integration
// +build integration

package integration_test

// Who links a doc or board to a project or a task, and unlinks it, through the
// routes the app's router serves (POST /link/add and /link/remove) with the
// body the web sends (services/entityLinkService.ts). On a project, whoever
// may add files to it: Work Access add_files, its members and admins, never
// its viewers or commenters. On a task, whoever may change that task, as its
// project's "Who can change tasks" decides. Someone outside the project does
// neither. A refusal changes nothing: the links read back as they were.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestWhoLinksAndUnlinksOnAProjectOrATask$' -v

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWhoLinksAndUnlinksOnAProjectOrATask(t *testing.T) {
	w := seedWorkWorld(t)
	// Changing a role or a project's policy refreshes cached profiles,
	// records the change and tells the people it touches.
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	// The routes, as the app's router serves them.
	routes := map[string]http.HandlerFunc{}
	_ = chi.Walk(router.Routes().(chi.Routes), func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch method + " " + route {
		case "POST /link/add", "POST /link/remove", "GET /link/source/{source_type}/{source_uuid}":
			routes[method+" "+route] = h.ServeHTTP
		}
		return nil
	})
	add, remove, list := routes["POST /link/add"], routes["POST /link/remove"], routes["GET /link/source/{source_type}/{source_uuid}"]
	if add == nil || remove == nil || list == nil {
		t.Fatalf("the link routes: %d of 3 served", len(routes))
	}

	sketches, brief := uuid.NewString(), uuid.NewString()
	live := "0001-01-01T00:00:00Z"
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:sketches", "dgraph.type": "Board", "board_uuid": sketches, "board_title": "Sketches", "board_private": false, "board_deleted_at": live},
		{"uid": "_:brief", "dgraph.type": "Board", "board_uuid": brief, "board_title": "Launch brief", "board_private": false, "board_deleted_at": live},
	})

	// change links or unlinks a board on a source as who, as the web does.
	change := func(who string, h http.HandlerFunc, sourceType, source, board string) (int, string) {
		return w.jfCall(who, h, http.MethodPost, "/", nil, map[string]string{
			"source_type": sourceType, "source_uuid": source, "ref_type": "board", "ref_uuid": board})
	}
	expect := func(t *testing.T, what string, code int, body string, want int) {
		t.Helper()
		if code != want {
			t.Errorf("%s: %d %s, want %d", what, code, body, want)
		}
	}
	// boards is what the source links, read back by ana, the project's admin.
	boards := func(t *testing.T, sourceType, source string) []string {
		t.Helper()
		code, body := w.jfCall("ana", list, http.MethodGet, "/", map[string]string{"source_type": sourceType, "source_uuid": source}, nil)
		var got struct {
			Data struct {
				Boards []struct {
					UUID string `json:"board_uuid"`
				} `json:"boards"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("the %s's links: %d %s", sourceType, code, body)
		}
		out := []string{}
		for _, b := range got.Data.Boards {
			out = append(out, b.UUID)
		}
		slices.Sort(out)
		return out
	}
	unchanged := func(t *testing.T, what, sourceType, source string, want ...string) {
		t.Helper()
		slices.Sort(want)
		if got := boards(t, sourceType, source); !slices.Equal(got, want) {
			t.Errorf("%s: the %s links %v, want %v", what, sourceType, got, want)
		}
	}

	code, body := change("ana", add, "project", w.project, sketches)
	expect(t, "ana, the project's admin, links a board to it", code, body, http.StatusOK)
	unchanged(t, "after ana links it", "project", w.project, sketches)

	t.Run("on a project, a viewer or a commenter neither links nor unlinks", func(t *testing.T) {
		for _, to := range []string{"viewer", "commenter"} {
			if code, body := w.setRole("ana", "ben", to); code != http.StatusOK {
				t.Fatalf("making ben a %s: %d %s", to, code, body)
			}
			code, body := change("ben", add, "project", w.project, brief)
			expect(t, "a "+to+" linking a board to the project", code, body, http.StatusForbidden)
			code, body = change("ben", remove, "project", w.project, sketches)
			expect(t, "a "+to+" unlinking the project's board", code, body, http.StatusForbidden)
			unchanged(t, "after a "+to+"'s refusals", "project", w.project, sketches)
		}
		if code, body := w.setRole("ana", "ben", "member"); code != http.StatusOK {
			t.Fatalf("making ben a member again: %d %s", code, body)
		}
	})

	t.Run("on a project, its members and admins link and unlink", func(t *testing.T) {
		code, body := change("ben", add, "project", w.project, brief)
		expect(t, "a member linking a board to the project", code, body, http.StatusOK)
		unchanged(t, "after the member links it", "project", w.project, sketches, brief)
		code, body = change("ben", remove, "project", w.project, brief)
		expect(t, "the member unlinking it", code, body, http.StatusOK)
		code, body = change("fay", remove, "project", w.project, sketches)
		expect(t, "an admin outside the member list unlinking ana's board", code, body, http.StatusOK)
		unchanged(t, "after both unlink", "project", w.project)
		code, body = change("fay", add, "project", w.project, sketches)
		expect(t, "the admin linking it again", code, body, http.StatusOK)
		unchanged(t, "after the admin links it again", "project", w.project, sketches)
	})

	t.Run("on a task, whoever may change that task", func(t *testing.T) {
		// Members change only the tasks assigned to them: ben has mine, not
		// task, and still adds files to the project.
		if code, body := w.setEditWho("ana", "assigned"); code != http.StatusOK {
			t.Fatalf("members on the tasks assigned to them: %d %s", code, body)
		}
		code, body := change("ana", add, "task", w.task, sketches)
		expect(t, "ana linking a board to her task", code, body, http.StatusOK)

		code, body = change("ben", add, "task", w.task, brief)
		expect(t, "a member linking a board to a task he may not change", code, body, http.StatusForbidden)
		code, body = change("ben", remove, "task", w.task, sketches)
		expect(t, "a member unlinking a board from a task he may not change", code, body, http.StatusForbidden)
		unchanged(t, "after the member's refusals", "task", w.task, sketches)

		code, body = change("ben", add, "task", w.mine, brief)
		expect(t, "a member linking a board to the task assigned to him", code, body, http.StatusOK)
		unchanged(t, "after he links it", "task", w.mine, brief)
		code, body = change("ben", remove, "task", w.mine, brief)
		expect(t, "and unlinking it", code, body, http.StatusOK)
		unchanged(t, "after he unlinks it", "task", w.mine)

		code, body = change("ben", add, "project", w.project, brief)
		expect(t, "the same member, linking on the project, adds a file to it", code, body, http.StatusOK)
		code, body = change("ben", remove, "project", w.project, brief)
		expect(t, "and unlinks it", code, body, http.StatusOK)
		if code, body := w.setEditWho("ana", nil); code != http.StatusOK {
			t.Fatalf("back to the workspace's choice: %d %s", code, body)
		}

		// By default ("members") a member changes any of the project's
		// tasks, not only the ones assigned to him.
		code, body = change("ben", add, "task", w.task, brief)
		expect(t, "by default, a member linking a board to someone else's task", code, body, http.StatusOK)
		unchanged(t, "after he links it", "task", w.task, sketches, brief)
		code, body = change("ben", remove, "task", w.task, brief)
		expect(t, "and unlinking it", code, body, http.StatusOK)
		unchanged(t, "after he unlinks it", "task", w.task, sketches)

		// Admins only: not even the task assigned to him.
		if code, body := w.setEditWho("ana", "admins"); code != http.StatusOK {
			t.Fatalf("admins only: %d %s", code, body)
		}
		code, body = change("ben", add, "task", w.mine, brief)
		expect(t, "with admins only, a member linking a board to the task assigned to him", code, body, http.StatusForbidden)
		code, body = change("ana", add, "task", w.mine, brief)
		expect(t, "the admin linking it there", code, body, http.StatusOK)
		code, body = change("ben", remove, "task", w.mine, brief)
		expect(t, "with admins only, the member unlinking it", code, body, http.StatusForbidden)
		unchanged(t, "after the member's refusals", "task", w.mine, brief)
		code, body = change("ana", remove, "task", w.mine, brief)
		expect(t, "the admin unlinking it", code, body, http.StatusOK)
		unchanged(t, "after the admin unlinks it", "task", w.mine)
		if code, body := w.setEditWho("ana", nil); code != http.StatusOK {
			t.Fatalf("back to the workspace's choice: %d %s", code, body)
		}
	})

	t.Run("someone outside the project does neither", func(t *testing.T) {
		code, body := change("cy", add, "project", w.project, brief)
		expect(t, "someone in the team, not the project, linking on it", code, body, http.StatusForbidden)
		code, body = change("cy", remove, "project", w.project, sketches)
		expect(t, "and unlinking on it", code, body, http.StatusForbidden)
		unchanged(t, "after the outsider's refusals", "project", w.project, sketches)

		code, body = change("cy", add, "task", w.task, brief)
		expect(t, "someone outside the project linking on its task", code, body, http.StatusForbidden)
		code, body = change("cy", remove, "task", w.task, sketches)
		expect(t, "and unlinking on it", code, body, http.StatusForbidden)
		// eve is the assignee of one of its tasks from outside it: she reads
		// that task, and changes none of its links.
		code, body = change("eve", add, "task", w.assigned, brief)
		expect(t, "a task's assignee from outside the project linking on it", code, body, http.StatusForbidden)
		unchanged(t, "after the outsiders' refusals", "task", w.task, sketches)
		unchanged(t, "after the assignee's refusal", "task", w.assigned)
	})

	t.Run("a source that isn't there, or is neither a project nor a task, takes no link", func(t *testing.T) {
		code, body := change("ana", add, "project", uuid.NewString(), brief)
		expect(t, "linking on a project that isn't there", code, body, http.StatusForbidden)
		code, body = change("ana", add, "task", uuid.NewString(), brief)
		expect(t, "linking on a task that isn't there", code, body, http.StatusForbidden)
		code, body = change("ana", add, "team", w.team, brief)
		expect(t, "linking on a team", code, body, http.StatusForbidden)
		code, body = change("ana", remove, "team", w.team, brief)
		expect(t, "unlinking on a team", code, body, http.StatusForbidden)
	})
}
