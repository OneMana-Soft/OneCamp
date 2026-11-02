package business_test

// Settings > Archive restores an item by the id its list gave (V-141, main's
// ruling of 31 Oct 2026): every kind's list and its restore agree on one id,
// a recording's being its key (an egress id, or a call's session key for a
// transcript-only node) rather than a uuid it never had. Each kind reads its
// own ids strictly, and looks them up in this workspace before it changes
// anything: an id of the wrong shape is 400, one that names nothing here is
// 404 and nothing is restored, and only a workspace admin gets that far.
//
// These drive the archive's own handlers, as the router chains them behind
// the admin gate, over an archive held in memory (export_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	archive "github.com/akashc777/OneCamp/business/Archive"
	archiveController "github.com/akashc777/OneCamp/controllers/Archive"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// archiveKinds is every kind the archive keeps, each with an id of the shape
// its list gives (id) and another of that shape that this workspace has
// nothing for (other). LiveKit writes an egress id as EG_ and twelve letters
// and digits, and a room's session id as RM_ and twelve; a call's session key
// is "call-" and that session id.
var archiveKinds = []struct {
	name, kind, id, other string
}{
	{"posts", "posts", "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b", "0b9e5c1d-2f3a-4b5c-9d6e-7f8a9b0c1d2e"},
	{"chats", "chats", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"},
	{"tasks", "tasks", "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e", "8e7d6c5b-4a39-4281-9706-5f4e3d2c1b0a"},
	{"attachments", "attachments", "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f", "7d6c5b4a-3928-4170-8695-4e3d2c1b0a9f"},
	{"docs", "docs", "4d5e6f7a-8b9c-4d0e-9f2a-3b4c5d6e7f8a", "6c5b4a39-2817-4069-9584-3d2c1b0a9f8e"},
	{"a recording, by its egress id", "recordings", "EG_9xKpQ2mWv7Ld", "EG_3vRt7YbNk5Hq"},
	{"a call's transcript, by its session key", "recordings", "call-RM_4fHs8TjNq2Wz", "call-RM_8mPw3ZcXr6Ty"},
}

// archiveRoutes are the archive's list and restore as the router chains them:
// the sign-in chain puts who is asking in the request (as, or nobody for
// nil), the admin gate, then the handlers.
func archiveRoutes(as *userModel.UserInfo) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if as != nil {
				req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, *as))
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Use(customMiddleware.VerifyAdminAuthOnlyPostgres)
	r.Get("/admin/archive/recent-items/{entityType}", archiveController.HandleGetRecentArchivedItems)
	r.Post("/admin/archive/restore", archiveController.HandleRestore)
	return r
}

// person is a signed-in member of the workspace, an admin or not.
func person(admin bool) *userModel.UserInfo {
	u := &userModel.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "sam", UserName: "Sam Rivera"}}
	u.UserPostgresInfo.Id = uuid.MustParse("5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b")
	u.UserPostgresInfo.IsAdmin = admin
	return u
}

// listed is one item of an archive list, as the web reads it.
type listed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// listOf asks for kind's archive list, as the restore dialog does.
func listOf(t *testing.T, h http.Handler, kind string) []listed {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/archive/recent-items/"+kind, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the %s list answered %d: %s", kind, rec.Code, rec.Body)
	}
	var body struct {
		Items []listed `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the %s list's body %q: %v", kind, rec.Body, err)
	}
	return body.Items
}

// restoreOf asks for ids of kind back, as the restore dialog does, and
// answers the status and the body (nil when it has none).
func restoreOf(t *testing.T, h http.Handler, kind string, ids []string) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"entity_type": kind, "entity_ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/archive/restore", bytes.NewReader(b)))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// ONE round trip over every kind: what the list gives, the restore takes, and
// brings back exactly that item. A recording's list gave its key, which the
// restore refused as no uuid: an admin could see a recorded call in Archive
// and never bring it back.
func TestTheArchiveRestoresEveryKindByTheIdItsListGave(t *testing.T) {
	for _, k := range archiveKinds {
		t.Run(k.name, func(t *testing.T) {
			f := archive.UseFakeArchive(t)
			f.Archive(k.kind, k.id, time.Now().Add(-time.Hour))
			h := archiveRoutes(person(true))

			items := listOf(t, h, k.kind)
			if len(items) != 1 || items[0].ID != k.id {
				t.Fatalf("the %s list gave %+v, want the one archived, %s", k.kind, items, k.id)
			}
			if strings.HasPrefix(k.id, "call-") && !strings.Contains(strings.ToLower(items[0].Name), "transcript") {
				t.Errorf("a call's transcript is listed as %q: it should say it is a transcript", items[0].Name)
			}

			code, body := restoreOf(t, h, k.kind, []string{items[0].ID})
			if code != http.StatusOK || body["count"] != float64(1) {
				t.Fatalf("restoring the %s its list gave answered %d %v, want 200 and a count of 1", k.kind, code, body)
			}
			if got := f.Writes(k.kind); !slices.Equal(got, []string{k.id}) {
				t.Errorf("the restore wrote %q back, want exactly the listed %s", got, k.id)
			}
			if !f.IsLive(k.kind, k.id) {
				t.Errorf("%s is still archived after its restore answered 200", k.id)
			}
		})
	}
}

// Each kind reads its own ids strictly: a uuid kind takes uuids, and a
// recording its key, an egress id or a call's session key, and nothing else.
// Anything else is 400 and nothing is looked up or written; so is a kind the
// archive doesn't keep.
func TestEachKindRefusesAnIdOfTheWrongShape(t *testing.T) {
	for _, c := range []struct{ name, kind, id string }{
		{"a post's id that isn't a uuid", "posts", "not-a-uuid"},
		{"a chat's id that is a number", "chats", "12345"},
		{"an empty task id", "tasks", ""},
		{"an attachment's id cut short", "attachments", "3c4d5e6f-7a8b-4c9d-8e1f"},
		{"a doc's id that is a recording's key", "docs", "EG_9xKpQ2mWv7Ld"},
		{"a recording's id that is a uuid", "recordings", "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"},
		{"an egress id cut short", "recordings", "EG_9xKpQ2m"},
		{"an egress id with a dash", "recordings", "EG_9xKpQ2mWv7L-"},
		{"an egress id with its prefix lower-cased", "recordings", "eg_9xKpQ2mWv7Ld"},
		{"an egress id with a space before it", "recordings", " EG_9xKpQ2mWv7Ld"},
		{"an egress id with a line after it", "recordings", "EG_9xKpQ2mWv7Ld\n"},
		{"a room's id that isn't a call's key", "recordings", "RM_4fHs8TjNq2Wz"},
		{"a call key made from an egress id", "recordings", "call-EG_9xKpQ2mWv7Ld"},
		{"a call key with no session", "recordings", "call-RM_"},
		{"a kind the archive doesn't keep", "widgets", "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := archive.UseFakeArchive(t)
			code, body := restoreOf(t, archiveRoutes(person(true)), c.kind, []string{c.id})
			if code != http.StatusBadRequest {
				t.Errorf("restoring %q as %s answered %d %v, want 400", c.id, c.kind, code, body)
			}
			if got := f.Writes(c.kind); len(got) > 0 {
				t.Errorf("a refused restore wrote %q", got)
			}
		})
	}
}

// Never the client's word for it: each kind looks its ids up in this
// workspace's own stores. An id of the right shape that names nothing here
// (another workspace's recording, a purged item) is 404, and a batch with one
// such id restores none of it.
func TestAnIdThatNamesNothingHereIsNotFoundAndNothingIsRestored(t *testing.T) {
	for _, k := range archiveKinds {
		t.Run(k.name, func(t *testing.T) {
			f := archive.UseFakeArchive(t)
			h := archiveRoutes(person(true))
			if code, body := restoreOf(t, h, k.kind, []string{k.other}); code != http.StatusNotFound {
				t.Errorf("restoring %s, which names nothing here, answered %d %v, want 404", k.other, code, body)
			}

			f.Archive(k.kind, k.id, time.Now().Add(-time.Hour))
			if code, body := restoreOf(t, h, k.kind, []string{k.id, k.other}); code != http.StatusNotFound {
				t.Errorf("restoring %s with %s, which names nothing here, answered %d %v, want 404", k.id, k.other, code, body)
			}
			if got := f.Writes(k.kind); len(got) > 0 {
				t.Errorf("a refused restore wrote %q", got)
			}
			if f.IsLive(k.kind, k.id) {
				t.Errorf("%s came back from a restore that was refused", k.id)
			}
		})
	}
}

// The route's own rule: only a workspace admin restores from the archive. A
// member gets the admin gate's 403, and nobody signed in its 401, before
// anything is read or written.
func TestOnlyAWorkspaceAdminRestoresFromTheArchive(t *testing.T) {
	for _, k := range archiveKinds {
		t.Run(k.name, func(t *testing.T) {
			f := archive.UseFakeArchive(t)
			f.Archive(k.kind, k.id, time.Now().Add(-time.Hour))

			code, body := restoreOf(t, archiveRoutes(person(false)), k.kind, []string{k.id})
			if code != http.StatusForbidden || body["code"] != customMiddleware.AdminRequired {
				t.Errorf("a member restoring %s answered %d %v, want 403 %s", k.id, code, body, customMiddleware.AdminRequired)
			}
			if code, _ := restoreOf(t, archiveRoutes(nil), k.kind, []string{k.id}); code != http.StatusUnauthorized {
				t.Errorf("nobody signed in restoring %s answered %d, want 401", k.id, code)
			}
			if got := f.Writes(k.kind); len(got) > 0 || f.IsLive(k.kind, k.id) {
				t.Errorf("a refused caller brought %s back (writes %q)", k.id, got)
			}
		})
	}
}
