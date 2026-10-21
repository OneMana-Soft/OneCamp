//go:build integration

package integration_test

// PR2 (the BE audit's bulk writes): one change to many tasks in one request,
// read, checked and, for a priority or a tag, written with a fixed number of
// Postgres statements whatever the number of tasks; a task the caller can't
// see is refused as not there; a change over 500 tasks is refused whole.
// Run: integ_rel.sh <wt> <log> 'TestPR2'

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	taskBulkController "github.com/akashc777/OneCamp/controllers/TaskBulk"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type pr2Answer struct {
	Data struct {
		Changed   []string `json:"changed"`
		Unchanged []string `json:"unchanged"`
		Refused   []struct {
			TaskUUID string `json:"task_uuid"`
			Msg      string `json:"msg"`
		} `json:"refused"`
	} `json:"data"`
	Msg string `json:"msg"`
}

func pr2Bulk(t *testing.T, s *wa2Scale, who string, ids []string, op, value string) (int, pr2Answer) {
	t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people[who])
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", who, err)
	}
	gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people[who].String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's profile: %v", who, err)
	}
	body, _ := json.Marshal(map[string]any{"task_uuids": ids, "op": op, "value": value})
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, chi.NewRouteContext())
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(http.HandlerFunc(taskBulkController.Bulk)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)).WithContext(c))
	var a pr2Answer
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	return rec.Code, a
}

func pr2Priorities(t *testing.T, s *wa2Scale, ids []string) map[string]string {
	t.Helper()
	list, _ := json.Marshal(ids)
	var out struct {
		Q []struct {
			UUID     string `json:"task_uuid"`
			Priority string `json:"task_priority"`
			Label    string `json:"task_label"`
		} `json:"q"`
	}
	if err := json.Unmarshal(s.dg.Query(t, `{ q(func: eq(task_uuid, `+string(list)+`)) { task_uuid task_priority task_label } }`), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, q := range out.Q {
		got[q.UUID] = q.Priority + "|" + q.Label
	}
	return got
}

func TestPR2ABulkChangeIsOneRequestWithAFixedCost(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(10)
	// A first change warms what is kept a while (the project's policies, keys).
	pr2Bulk(t, s, "ana", s.tasks, "priority", "medium")

	counter.Reset()
	code, a := pr2Bulk(t, s, "ana", s.tasks, "priority", "high")
	few := counter.Count()
	if code != http.StatusOK || len(a.Data.Changed)+len(a.Data.Unchanged) != 10 || len(a.Data.Refused) != 0 {
		t.Fatalf("ana sets ten tasks high: %d %+v", code, a)
	}
	for id, v := range pr2Priorities(t, s, s.tasks) {
		if !strings.HasPrefix(v, "high|") {
			t.Fatalf("%s is %s", id, v)
		}
	}

	s.addTasks(490)
	counter.Reset()
	code, a = pr2Bulk(t, s, "ana", s.tasks, "priority", "low")
	many := counter.Count()
	if code != http.StatusOK || len(a.Data.Changed) != 500 {
		t.Fatalf("ana sets 500 tasks low: %d changed %d, refused %v", code, len(a.Data.Changed), a.Data.Refused)
	}
	t.Logf("a bulk priority sends Postgres %d statements for 10 tasks, %d for 500", few, many)
	if many != few || many > 12 {
		t.Fatalf("%d statements for 500 tasks, %d for 10 (at most 12)", many, few)
	}

	t.Run("a tag is added to each task's own", func(t *testing.T) {
		code, a := pr2Bulk(t, s, "ben", s.tasks[:3], "tag_add", "Launch")
		if code != http.StatusOK || len(a.Data.Changed) != 3 {
			t.Fatalf("%d %+v", code, a)
		}
		for id, v := range pr2Priorities(t, s, s.tasks[:3]) {
			if !strings.HasSuffix(v, "|launch") && !strings.HasSuffix(v, "|Launch") {
				t.Fatalf("%s: %s", id, v)
			}
		}
	})

	t.Run("a task the caller can't see is refused as not there", func(t *testing.T) {
		if !s.restrict(1) { // task 2, made by ana, kept from ben
			t.Fatal("no task_restrictions table")
		}
		defer s.restrict(0)
		code, a := pr2Bulk(t, s, "ben", s.tasks[:3], "priority", "medium")
		if code != http.StatusOK || len(a.Data.Refused) != 1 || a.Data.Refused[0].TaskUUID != s.tasks[2] || a.Data.Refused[0].Msg != "That task doesn't exist or you can't see it." {
			t.Fatalf("%d %+v", code, a)
		}
		if v := pr2Priorities(t, s, s.tasks[2:3])[s.tasks[2]]; strings.HasPrefix(v, "medium|") {
			t.Fatalf("the hidden task changed: %s", v)
		}
	})

	t.Run("many tasks are made in one request, in order", func(t *testing.T) {
		ctx := context.Background()
		pg, _ := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people["ben"])
		gr, _ := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people["ben"].String())
		body, _ := json.Marshal(map[string]any{"project_uuid": s.project, "tasks": []map[string]string{{"task_name": "Sticky one"}, {"task_name": "Sticky two", "task_priority": "high"}, {"task_name": "Sticky three"}}})
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, chi.NewRouteContext())
		rec := httptest.NewRecorder()
		customMiddleware.RequestIDMiddleware(http.HandlerFunc(taskBulkController.CreateTasks)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)).WithContext(c))
		var got struct {
			Data struct {
				Made []string `json:"made"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Data.Made) != 3 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		names := pr2Names(t, s, got.Data.Made)
		if names[got.Data.Made[0]] != "Sticky one" || names[got.Data.Made[1]] != "Sticky two" || names[got.Data.Made[2]] != "Sticky three" {
			t.Fatalf("made in order: %v", names)
		}
	})

	t.Run("more than 500 is refused whole", func(t *testing.T) {
		s.addTasks(1)
		if code, a := pr2Bulk(t, s, "ana", s.tasks, "priority", "high"); code != http.StatusBadRequest || a.Msg != "Change at most 500 tasks at a time." {
			t.Fatalf("%d %+v", code, a)
		}
	})
}

func pr2Names(t *testing.T, s *wa2Scale, ids []string) map[string]string {
	t.Helper()
	list, _ := json.Marshal(ids)
	var out struct {
		Q []struct {
			UUID string `json:"task_uuid"`
			Name string `json:"task_name"`
		} `json:"q"`
	}
	if err := json.Unmarshal(s.dg.Query(t, `{ q(func: eq(task_uuid, `+string(list)+`)) { task_uuid task_name } }`), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, q := range out.Q {
		got[q.UUID] = q.Name
	}
	return got
}
