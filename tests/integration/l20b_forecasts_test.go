//go:build integration

package integration_test

// linear-20, head 2: every listed project's forecast in ONE read
// (GET /project/forecasts?ids=&tz=), for the projects overview and the goal
// rows. Each answer is the single route's own forecast (one function, so the
// two can't drift), and only for projects the caller may read: an id they
// can't, one that names nothing and one that isn't an id are left out without
// a word. Run: integ_rel.sh <wt> <log>
// '^(TestLinear20BForecastsInOneRead|TestLinear20BForecastsCapTheIds)$'

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

// forecasts reads ids' forecasts as who (through a token when token), and
// answers the HTTP code, each project's forecast by id, and the raw body.
func (w *dg10World) forecasts(who string, token bool, ids ...string) (int, map[string]map[string]any, string) {
	w.t.Helper()
	target := "/?tz=UTC&ids=" + url.QueryEscape(strings.Join(ids, ","))
	code, body := w.call(who, token, taskController.ProjectForecasts, http.MethodGet, target, nil, nil)
	var out struct {
		Data struct {
			Forecasts map[string]map[string]any `json:"forecasts"`
		} `json:"data"`
	}
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			w.t.Fatalf("%s: %v", body, err)
		}
	}
	return code, out.Data.Forecasts, body
}

// single is the one-project route's forecast, as the batch must answer it.
func (w *dg10World) single(who, project string) map[string]any {
	w.t.Helper()
	code, body := w.call(who, false, taskController.ProjectForecast, http.MethodGet, "/?tz=UTC", map[string]string{"project_uuid": project}, nil)
	if code != http.StatusOK {
		w.t.Fatalf("single forecast of %s as %s: %d %s", project, who, code, body)
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		w.t.Fatal(err)
	}
	return out.Data
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestLinear20BForecastsInOneRead(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.task("Draft the brief", w.p, 3, 6, "ana")
	w.task("Book the hall", w.p, 4, 9, "ben", "Draft the brief")
	w.task("Plan the posts", w.q, 2, 8, "cat")
	p, q := strings.ToLower(w.p), strings.ToLower(w.q)
	nobody := uuid.NewString()

	// A member of both: both forecasts, each exactly the single route's.
	code, got, body := w.forecasts("ana", false, w.p, w.q)
	if code != http.StatusOK {
		t.Fatalf("ana: %d %s", code, body)
	}
	if want := []string{p, q}; !reflect.DeepEqual(keysOf(got), sortedCopy(want)) {
		t.Fatalf("ana's forecasts: %v, want %v", keysOf(got), sortedCopy(want))
	}
	for id, project := range map[string]string{p: w.p, q: w.q} {
		if one := w.single("ana", project); !reflect.DeepEqual(got[id], one) {
			t.Errorf("the batch's forecast of %s drifted from the single route's:\n batch  %v\n single %v", id, got[id], one)
		}
	}

	// ben is in Launch, not Marketing: only Launch, and nothing says why the
	// rest are missing (not Marketing, not an id that names nothing, not a
	// string that isn't an id). Asked twice, answered once.
	code, got, body = w.forecasts("ben", false, w.p, w.q, nobody, "not-an-id", w.p)
	if code != http.StatusOK {
		t.Fatalf("ben: %d %s", code, body)
	}
	if !reflect.DeepEqual(keysOf(got), []string{p}) {
		t.Fatalf("ben's forecasts: %v, want only Launch %s", keysOf(got), p)
	}
	if strings.Contains(body, w.q) || strings.Contains(body, "Marketing") || strings.Contains(body, nobody) {
		t.Errorf("ben's answer names what he can't read: %s", body)
	}
	if one := w.single("ben", w.p); !reflect.DeepEqual(got[p], one) {
		t.Errorf("ben's Launch forecast drifted from the single route's")
	}

	// A token acts as its member: the same answer, never more.
	code, got, _ = w.forecasts("ben", true, w.p, w.q)
	if code != http.StatusOK || !reflect.DeepEqual(keysOf(got), []string{p}) {
		t.Fatalf("ben's token: %d %v, want only Launch", code, keysOf(got))
	}

	// cat is Launch's viewer: a viewer reads a forecast, as on the single route.
	code, got, _ = w.forecasts("cat", false, w.p)
	if code != http.StatusOK || !reflect.DeepEqual(keysOf(got), []string{p}) {
		t.Fatalf("cat (viewer): %d %v, want Launch", code, keysOf(got))
	}

	// Nothing asked, or nothing readable: an empty object, never null.
	for _, ids := range [][]string{nil, {nobody}} {
		code, _, body = w.forecasts("ben", false, ids...)
		// The answer is indented: compared without its spaces.
		var flat bytes.Buffer
		if code != http.StatusOK || json.Compact(&flat, []byte(body)) != nil || !strings.Contains(flat.String(), `"forecasts":{}`) {
			t.Errorf("ben asking %v: %d %s, want 200 with an empty forecasts object", ids, code, body)
		}
	}
}

func TestLinear20BForecastsCapTheIds(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	ids := []string{w.p}
	for len(ids) < 100 {
		ids = append(ids, uuid.NewString())
	}
	// 100 ids, one readable: answered.
	code, got, body := w.forecasts("ana", false, ids...)
	if code != http.StatusOK || !reflect.DeepEqual(keysOf(got), []string{strings.ToLower(w.p)}) {
		t.Fatalf("100 ids: %d %v %s, want Launch alone", code, keysOf(got), body)
	}
	// 101 distinct ids: refused whole, with words, and nothing is read.
	code, _, body = w.forecasts("ana", false, append(ids, uuid.NewString())...)
	if code != http.StatusBadRequest || !strings.Contains(body, "100") {
		t.Fatalf("101 ids: %d %s, want 400 naming the cap of 100", code, body)
	}
	// Repeats don't count against the cap: 100 distinct ids, sent twice over.
	code, _, body = w.forecasts("ana", false, append(ids, ids...)...)
	if code != http.StatusOK {
		t.Fatalf("100 distinct ids sent twice: %d %s, want 200", code, body)
	}
}
