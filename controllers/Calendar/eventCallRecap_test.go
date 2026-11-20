package Calendar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// Make task answers each outcome in its own status: a new task 201, the
// item's own task 200, the calendar's refusals as every change is refused, a
// project the person can't add to 403, an item the event doesn't have 404,
// one being made 409, and a request with no project 400, before anything.
func TestMakeTaskAnswersEachOutcome(t *testing.T) {
	was := makeEventActionTask
	t.Cleanup(func() { makeEventActionTask = was })
	var asked int
	answer := func(task *business.ActionTask, err error) {
		makeEventActionTask = func(_ context.Context, _ *model.UserInfo, id, key string, i int, project string) (*business.ActionTask, error) {
			asked++
			if id != "e1" || key != "call:s1" || i != 1 || project != "p1" {
				t.Errorf("asked %s %s %d %s", id, key, i, project)
			}
			return task, err
		}
	}
	ask := func(index, body string) int {
		r := httptest.NewRequest(http.MethodPost, "/calendar/events/e1/call/artefacts/call:s1/action-items/"+index+"/task", strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("eventId", "e1")
		rc.URLParams.Add("key", "call:s1")
		rc.URLParams.Add("index", index)
		ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, model.UserInfo{})
		w := httptest.NewRecorder()
		MakeCalendarEventCallActionTask(w, r.WithContext(ctx))
		return w.Code
	}
	for _, c := range []struct {
		task *business.ActionTask
		err  error
		want int
	}{
		{&business.ActionTask{TaskUUID: "t1", Made: true, Linked: true}, nil, http.StatusCreated},
		{&business.ActionTask{TaskUUID: "t1"}, nil, http.StatusOK},
		{nil, business.ErrOnlyCreatorChanges, http.StatusForbidden},
		{nil, business.ErrEventNotFound, http.StatusNotFound},
		{nil, fmt.Errorf("%w: project 'Payroll'", business.ErrCantAddTasks), http.StatusForbidden},
		{nil, business.ErrNoActionItem, http.StatusNotFound},
		{nil, business.ErrActionItemBusy, http.StatusConflict},
	} {
		answer(c.task, c.err)
		if code := ask("1", `{"project_uuid":"p1"}`); code != c.want {
			t.Errorf("%v %v: %d, want %d", c.task, c.err, code, c.want)
		}
	}
	answer(nil, errors.New("never asked"))
	asked = 0
	for index, body := range map[string]string{"1": `{}`, "x": `{"project_uuid":"p1"}`, "-1": `{"project_uuid":"p1"}`} {
		if code := ask(index, body); code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Errorf("index %s body %s: %d", index, body, code)
		}
	}
	if asked != 0 {
		t.Errorf("a request with no project or no item was asked %d times", asked)
	}
}
