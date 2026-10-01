package journey

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A fake instance, so the runner's own behaviour can be tested without one.
//
// What is worth pinning here is not that a happy path passes. It is the two
// silent failures this command exists to catch, and the two ways the REPORT
// could lie: a skipped write reported as a pass, and four cascading failures
// hiding which one came first.

type fakeInstance struct {
	// swallowWrites makes the instance accept a task and then never return it,
	// which is exactly what a filter matching nothing looks like from outside.
	swallowWrites bool
	// searchBlind makes search return nothing while everything else works: a
	// live index that documents are not reaching.
	searchBlind bool
	created     []string
}

func (f *fakeInstance) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, data interface{}) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"message": "ok", "data": data})
	}

	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"msg": "bad token"})
			return
		}
		write(w, map[string]interface{}{"id": "11111111-2222-3333-4444-555555555555"})
	})
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]string{{"project_uuid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "name": "Demo"}})
	})
	mux.HandleFunc("/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.created = append(f.created, body["task_name"])
			write(w, map[string]string{"task_uuid": "99999999-8888-7777-6666-555555555555", "task_name": body["task_name"]})
			return
		}
		q := r.URL.Query().Get("search")
		out := []map[string]string{}
		if !f.swallowWrites {
			for _, name := range f.created {
				if q == "" || strings.Contains(name, q) {
					out = append(out, map[string]string{"task_name": name})
				}
			}
		}
		write(w, out)
	})
	mux.HandleFunc("/v1/tasks/", func(w http.ResponseWriter, r *http.Request) { write(w, map[string]string{"status": "done"}) })
	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		out := []map[string]string{}
		if !f.searchBlind && !f.swallowWrites {
			for _, name := range f.created {
				if strings.Contains(name, q) {
					out = append(out, map[string]string{"task_name": name})
				}
			}
		}
		write(w, out)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func runAgainst(t *testing.T, f *fakeInstance, project string) []result {
	t.Helper()
	srv := f.server(t)
	c := &client{BaseURL: srv.URL, Token: "test-token", HTTP: srv.Client()}
	st := &state{ProjectUUID: project, marker: "jc-test-marker"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return run(ctx, c, st, steps())
}

func byName(results []result, name string) result {
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	return result{Name: name, Detail: "step not present"}
}

func TestAWorkingInstancePassesEveryStep(t *testing.T) {
	results := runAgainst(t, &fakeInstance{}, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	for _, r := range results {
		if r.Outcome != outcomePassed {
			t.Errorf("%s: %s (%s)", r.Name, r.Outcome, r.Detail)
		}
	}
}

// The defect this command exists for: the write is accepted and the read cannot
// see it. Nothing errors, and a unit test on either side alone stays green.
func TestAWriteThatCannotBeReadBackIsCaught(t *testing.T) {
	results := runAgainst(t, &fakeInstance{swallowWrites: true}, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	if got := byName(results, "task-create").Outcome; got != outcomePassed {
		t.Fatalf("creating the task should still succeed, got %s", got)
	}
	readback := byName(results, "task-readback")
	if readback.Outcome != outcomeFailed {
		t.Fatalf("a task that cannot be read back must fail the readback, got %s", readback.Outcome)
	}
	if !strings.Contains(readback.Detail, "jc-test-marker") {
		t.Errorf("the failure must name the marker it looked for, got %q", readback.Detail)
	}
}

// Search returning nothing is indistinguishable from "no results" to a user, and
// the health page says in its own words that it cannot prove documents arrive.
func TestASearchThatFindsNothingIsCaught(t *testing.T) {
	results := runAgainst(t, &fakeInstance{searchBlind: true}, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	if got := byName(results, "task-readback").Outcome; got != outcomePassed {
		t.Fatalf("the readback should pass; only search is blind here, got %s", got)
	}
	if got := byName(results, "search-finds-it").Outcome; got != outcomeFailed {
		t.Errorf("a search that cannot find a just-created task must fail, got %s", got)
	}
}

// A skipped write must never read as a pass.
func TestWritingStepsAreSkippedWithoutAProjectAndNotPassed(t *testing.T) {
	results := runAgainst(t, &fakeInstance{}, "")

	for _, name := range []string{"task-create", "task-readback", "task-status", "search-finds-it"} {
		r := byName(results, name)
		if r.Outcome != outcomeSkipped {
			t.Errorf("%s must be skipped without a project, got %s", name, r.Outcome)
		}
	}
	// The read-only steps still ran, or the run would be worthless.
	if got := byName(results, "identity").Outcome; got != outcomePassed {
		t.Errorf("identity should still run without a project, got %s", got)
	}
}

// One fault must not be reported as four.
func TestLaterStepsAreNotReachedAfterAFailure(t *testing.T) {
	srv := (&fakeInstance{}).server(t)
	c := &client{BaseURL: srv.URL, Token: "wrong-token", HTTP: srv.Client()}
	st := &state{ProjectUUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", marker: "jc-test-marker"}

	results := run(context.Background(), c, st, steps())

	if got := byName(results, "identity").Outcome; got != outcomeFailed {
		t.Fatalf("a bad token must fail identity, got %s", got)
	}
	for _, name := range []string{"projects-readable", "task-create", "task-readback", "task-status", "search-finds-it"} {
		if got := byName(results, name).Outcome; got != outcomeNotReached {
			t.Errorf("%s should be reported as not reached, not run: got %s", name, got)
		}
	}
}

func TestFindUUIDPrefersANamedIdentifier(t *testing.T) {
	raw := json.RawMessage(`{"note":"aaaaaaaa-1111-2222-3333-444444444444","task_uuid":"99999999-8888-7777-6666-555555555555"}`)
	if got := findUUID(raw); got != "99999999-8888-7777-6666-555555555555" {
		t.Errorf("named identifier should win over any other uuid in the payload, got %q", got)
	}
}

// The exit contract, which is what cron and CI actually consume.
func TestTheReportExitsNonZeroOnlyWhenSomethingFailed(t *testing.T) {
	var buf bytes.Buffer

	passedAndSkipped := []result{
		{Name: "identity", Outcome: outcomePassed, Describe: "d"},
		{Name: "task-create", Outcome: outcomeSkipped, Describe: "d", Detail: "no project configured"},
	}
	if code := report(&buf, "https://example.test", passedAndSkipped); code != 0 {
		t.Errorf("passes and skips must exit 0, got %d", code)
	}
	// A skip has to be visible, or an operator reads a clean run as full coverage.
	if !strings.Contains(buf.String(), "skip") {
		t.Errorf("a skipped step must be shown as skipped:\n%s", buf.String())
	}

	buf.Reset()
	withFailure := append(passedAndSkipped, result{Name: "search-finds-it", Outcome: outcomeFailed, Describe: "d", Detail: "index empty"})
	if code := report(&buf, "https://example.test", withFailure); code != 1 {
		t.Errorf("a failure must exit 1, got %d", code)
	}
	if !strings.Contains(buf.String(), "index empty") {
		t.Errorf("the reason must reach the output:\n%s", buf.String())
	}
}

// The auth middleware and the tool handlers refuse with DIFFERENT envelopes.
// Reading only one of them printed raw JSON where a sentence belonged, and no
// unit test caught it because the fake server only ever spoke one dialect.
func TestBothRefusalEnvelopesReadAsASentence(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"middleware", `{"error":true,"message":"invalid or expired token"}`},
		{"tool handler", `{"msg":"invalid or expired token"}`},
	}

	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := &client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()}

		_, err := c.do(context.Background(), "GET", "/v1/me", nil)
		if err == nil {
			t.Fatalf("%s: a 401 must be an error", tc.name)
		}
		if !strings.Contains(err.Error(), "invalid or expired token") {
			t.Errorf("%s: the reason must be readable, got %q", tc.name, err.Error())
		}
		if strings.Contains(err.Error(), "{") {
			t.Errorf("%s: raw JSON leaked into the message: %q", tc.name, err.Error())
		}
		srv.Close()
	}
}
