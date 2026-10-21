//go:build integration
// +build integration

package integration_test

// Board columns in the project's order (design/projects-vs-asana-jira/
// BOARD_COLUMNS_PLAN.md), against real Postgres 12 and Dgraph, through the
// handlers: a project's admins set the order of its statuses, everyone reads
// it, open boards are told, a member and an admin's API token (a member
// there) are refused, Done and Cancelled stay after the open columns, and a
// status made or deleted later keeps the order whole.
//
// Run: go test -tags=integration ./tests/integration/ -run TestBoardColumns -v

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/akashc777/OneCamp/tests/integration"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

func TestBoardColumnsTakeTheProjectsOrder(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)

	// call runs a status handler as who (through an API token when token),
	// on the project, with a JSON body.
	call := func(who string, token bool, h http.HandlerFunc, params map[string]string, body any) (int, string) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("project_uuid", w.project)
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		if token {
			c = helpers.WithActsThroughToken(c)
		}
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", rd).WithContext(c))
		return rec.Code, compact(rec.Body.Bytes())
	}
	orderOf := func(who string) []string {
		t.Helper()
		code, out := call(who, false, taskStatusController.List, nil, nil)
		var got struct {
			Data struct {
				Order []string `json:"order"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); code != http.StatusOK || err != nil {
			t.Fatalf("statuses as %s: %d %s", who, code, out)
		}
		return got.Data.Order
	}
	create := func(name, category string) string {
		t.Helper()
		code, out := call("ana", false, taskStatusController.Create, nil, map[string]string{"name": name, "category": category, "color": "violet"})
		var got struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); code != http.StatusOK || err != nil || got.Data.ID == "" {
			t.Fatalf("make %s: %d %s", name, code, out)
		}
		return got.Data.ID
	}
	told := func() int {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		n := 0
		for _, m := range broker.got {
			var msg struct {
				Type int                            `json:"type"`
				Data mqttStruct.MqttProjectStatuses `json:"data"`
			}
			if m.topic == helpers.GetMqttTopicForProjectMessage(w.project) && json.Unmarshal(m.payload, &msg) == nil &&
				msg.Type == mqttStruct.MESSAGE_PROJECT_STATUSES && msg.Data.ProjectUuid == w.project {
				n++
			}
		}
		return n
	}

	qa := create("QA", "inReview")
	if got, want := orderOf("ben"), []string{"backlog", "todo", "inProgress", "inReview", qa, "done", "canceled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before any order: %v, want %v", got, want)
	}

	t.Run("its admins set the order, everyone reads it, open boards are told", func(t *testing.T) {
		before := told()
		want := []string{"backlog", "todo", "inReview", qa, "inProgress", "canceled", "done"}
		code, out := call("ana", false, taskStatusController.Order, nil, map[string]any{"order": want})
		if code != http.StatusOK || !strings.Contains(out, `"order":["backlog","todo","inReview","`+qa+`","inProgress","canceled","done"]`) {
			t.Fatalf("the admin's order: %d %s", code, out)
		}
		if got := orderOf("ben"); !reflect.DeepEqual(got, want) {
			t.Fatalf("a member reads %v, want %v", got, want)
		}
		if told() != before+1 {
			t.Errorf("open boards weren't told of the new order (%d messages before, %d after)", before, told())
		}
	})

	t.Run("a member, and an admin through an API token, may not", func(t *testing.T) {
		order := []string{"todo", "backlog", "inReview", qa, "inProgress", "canceled", "done"}
		for _, c := range []struct {
			who   string
			token bool
		}{{"ben", false}, {"ana", true}} {
			if code, out := call(c.who, c.token, taskStatusController.Order, nil, map[string]any{"order": order}); code != http.StatusForbidden || !strings.Contains(out, "Only this project's admins") {
				t.Errorf("%s (token %v): %d %s", c.who, c.token, code, out)
			}
			if code, out := call(c.who, c.token, taskStatusController.Reorder, nil, map[string]any{"ids": []string{qa}}); code != http.StatusForbidden {
				t.Errorf("%s (token %v) reorders: %d %s", c.who, c.token, code, out)
			}
		}
		if got := orderOf("ana"); got[0] != "backlog" {
			t.Fatalf("a refused order was saved: %v", got)
		}
	})

	t.Run("Done and Cancelled stay after the open columns, and every status is named once", func(t *testing.T) {
		for name, order := range map[string][]string{
			"Done first":      {"done", "backlog", "todo", "inReview", qa, "inProgress", "canceled"},
			"QA after Done":   {"backlog", "todo", "inReview", "inProgress", "canceled", "done", qa},
			"one left out":    {"backlog", "todo", "inReview", "inProgress", "canceled", "done"},
			"one named twice": {"backlog", "todo", "todo", "inReview", qa, "inProgress", "canceled", "done"},
		} {
			if code, out := call("ana", false, taskStatusController.Order, nil, map[string]any{"order": order}); code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, code, out)
			} else if name == "Done first" && !strings.Contains(out, "stay after the open ones") {
				t.Errorf("%s says %s", name, out)
			}
		}
	})

	t.Run("a status made later goes with its own; a deleted one leaves the order", func(t *testing.T) {
		ready := create("Ready", "inReview")
		if got, want := orderOf("ben"), []string{"backlog", "todo", "inReview", qa, ready, "inProgress", "canceled", "done"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("with Ready: %v, want %v", got, want)
		}
		// The statuses dialog's reorder keeps one order: Ready before QA, in
		// the places they hold.
		if code, out := call("ana", false, taskStatusController.Reorder, nil, map[string]any{"ids": []string{ready, qa}}); code != http.StatusOK {
			t.Fatalf("reorder: %d %s", code, out)
		}
		if got, want := orderOf("ben"), []string{"backlog", "todo", "inReview", ready, qa, "inProgress", "canceled", "done"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("after the dialog's reorder: %v, want %v", got, want)
		}
		before := told()
		if code, out := call("ana", false, taskStatusController.Delete, map[string]string{"status_id": qa}, map[string]string{}); code != http.StatusOK {
			t.Fatalf("delete QA: %d %s", code, out)
		}
		if got, want := orderOf("ben"), []string{"backlog", "todo", "inReview", ready, "inProgress", "canceled", "done"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("after QA is deleted: %v, want %v", got, want)
		}
		if told() <= before {
			t.Error("open boards weren't told QA was deleted")
		}
	})
}
