//go:build integration
// +build integration

package integration_test

// A restricted task's links are kept as the task is. Ben, a member of the
// project the task is kept from, neither lists what it links nor links or
// unlinks anything on it, and a board it links never names it to him; on a
// task he may see, he does all of it. Ana, who made it, and Fay, an admin of
// the project, list its links.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestARestrictedTasksLinksAreKeptAsTheTaskIs$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/taskread"
)

func TestARestrictedTasksLinksAreKeptAsTheTaskIs(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	// The task is ana's, kept to the project's admins and its maker: not to
	// ben, a member.
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(ctx, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	board, other := uuid.NewString(), uuid.NewString()
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Vendor shortlist", "board_private": false, "board_deleted_at": "0001-01-01T00:00:00Z"},
		{"uid": "_:o", "dgraph.type": "Board", "board_uuid": other, "board_title": "Sketches", "board_private": false, "board_deleted_at": "0001-01-01T00:00:00Z"},
	})
	call := func(who string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(c))
		return rec.Code, rec.Body.String()
	}
	link := func(task, ref string) map[string]string {
		return map[string]string{"source_type": "task", "source_uuid": task, "ref_type": "board", "ref_uuid": ref}
	}
	source := func(task string) map[string]string {
		return map[string]string{"source_type": "task", "source_uuid": task}
	}
	if code, body := call("ana", entityLinkController.AddLink, nil, link(w.task, board)); code != http.StatusOK {
		t.Fatalf("ana links the board to her task: %d %s", code, body)
	}

	t.Run("a task kept from ben keeps its links from him", func(t *testing.T) {
		if code, body := call("ben", entityLinkController.GetSourceLinks, source(w.task), nil); code != http.StatusForbidden || strings.Contains(body, board) {
			t.Errorf("ben listed the links of a task kept from him: %d %s", code, body)
		}
		if code, body := call("ben", entityLinkController.AddLink, nil, link(w.task, other)); code != http.StatusForbidden {
			t.Errorf("ben linked a board to a task kept from him: %d %s", code, body)
		}
		if code, body := call("ben", entityLinkController.RemoveLink, nil, link(w.task, board)); code != http.StatusForbidden {
			t.Errorf("ben unlinked a board from a task kept from him: %d %s", code, body)
		}
		if code, body := call("ben", entityLinkController.GetRefLinks, map[string]string{"ref_type": "board", "ref_uuid": board}, nil); code != http.StatusOK || strings.Contains(body, w.task) {
			t.Errorf("the board named to ben a task kept from him: %d %s", code, body)
		}
	})
	t.Run("on a task he may see, ben links and lists", func(t *testing.T) {
		if code, body := call("ben", entityLinkController.AddLink, nil, link(w.assigned, other)); code != http.StatusOK {
			t.Fatalf("ben links a board to the task he made: %d %s", code, body)
		}
		if code, body := call("ben", entityLinkController.GetSourceLinks, source(w.assigned), nil); code != http.StatusOK || !strings.Contains(body, other) {
			t.Errorf("ben lists the links of the task he made: %d %s", code, body)
		}
	})
	t.Run("its maker and the project's admin list its links", func(t *testing.T) {
		for _, who := range []string{"ana", "fay"} {
			if code, body := call(who, entityLinkController.GetSourceLinks, source(w.task), nil); code != http.StatusOK || !strings.Contains(body, board) {
				t.Errorf("%s lists the task's links: %d %s", who, code, body)
			}
		}
	})
	t.Run("a read that fails says to try again, and nothing of why", func(t *testing.T) {
		prev := taskread.Decide
		t.Cleanup(func() { taskread.Decide = prev })
		taskread.Decide = func(context.Context, string) (map[string]bool, error) {
			return nil, errors.New("the graph is unreachable at 10.0.0.7")
		}
		code, body := call("fay", entityLinkController.GetSourceLinks, source(w.task), nil)
		if code != http.StatusServiceUnavailable || strings.Contains(body, "10.0.0.7") {
			t.Errorf("a failed read: %d %s", code, body)
		}
	})
}
