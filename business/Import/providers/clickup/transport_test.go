package clickup

// Tests for the transport layer (getJSON) — same rationale as
// the Linear transport tests: catches API-shape drift without needing
// real ClickUp credentials in CI.
//
// Coverage:
//   - 200 → JSON decode
//   - 429 + Retry-After → ErrRateLimited
//   - 401 → "reconnect" error
//   - 404 → ErrAttachmentGone
//   - 5xx → status-coded error
//   - cancelled context → no network traffic
//   - Authorization header shape (raw token, no "Bearer " prefix)

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// withFakeClickUp swaps the package-level apiBase to point at a
// httptest server. Restored on test cleanup.
func withFakeClickUp(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	prev := apiBase
	apiBase = srv.URL
	t.Cleanup(func() {
		apiBase = prev
		srv.Close()
	})
	return srv
}

func TestGetJSON_HappyPath(t *testing.T) {
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		// ClickUp expects the raw token, NOT "Bearer <tok>". Verifying
		// this header shape is the most valuable single assertion in
		// these tests — the reverse pattern works on Linear and would
		// silently break on ClickUp.
		if got := r.Header.Get("Authorization"); got != "pk_test_123" {
			t.Errorf("clickup auth header should be the raw token; got %q", got)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/team" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"teams":[{"id":"123","name":"Acme"}]}`))
	})

	p := New()
	var out struct {
		Teams []struct {
			ID, Name string
		} `json:"teams"`
	}
	if err := p.getJSON(context.Background(), "pk_test_123", "/team", &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if len(out.Teams) != 1 || out.Teams[0].ID != "123" {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestGetJSON_StripsBearerPrefix(t *testing.T) {
	// If a caller accidentally hands us "Bearer <tok>" (because they
	// copied from a Linear path), the transport must strip the prefix
	// so ClickUp accepts the call. Belt-and-braces.
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "pk_real" {
			t.Errorf("expected stripped 'Bearer ' prefix; got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	p := New()
	var out struct{}
	if err := p.getJSON(context.Background(), "Bearer pk_real", "/team", &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
}

func TestGetJSON_429MapsToRateLimited(t *testing.T) {
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	p := New()
	var out struct{}
	err := p.getJSON(context.Background(), "tok", "/team", &out)
	if err == nil {
		t.Fatal("expected ErrRateLimited")
	}
	d, ok := importProvider.IsRateLimited(err)
	if !ok {
		t.Fatalf("not ErrRateLimited: %T %v", err, err)
	}
	if d != 12*time.Second {
		t.Fatalf("expected RetryAfter=12s, got %v", d)
	}
}

func TestGetJSON_401MapsToReconnect(t *testing.T) {
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := New()
	var out struct{}
	err := p.getJSON(context.Background(), "tok", "/team", &out)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "reconnect") {
		t.Fatalf("expected reconnect hint, got %v", err)
	}
}

func TestGetJSON_404MapsToErrAttachmentGone(t *testing.T) {
	// ClickUp returns 404 for deleted/never-existed entities. The
	// orchestrator's attachment worker watches for ErrAttachmentGone
	// to mark a placeholder + skip retries; mapping ALL 404s onto it
	// is the right call because the only thing the worker can do at
	// 404 is move on.
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	p := New()
	var out struct{}
	err := p.getJSON(context.Background(), "tok", "/list/missing/task", &out)
	if !errors.Is(err, importProvider.ErrAttachmentGone) {
		t.Fatalf("expected ErrAttachmentGone, got %v", err)
	}
}

func TestGetJSON_5xxIncludesStatus(t *testing.T) {
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	p := New()
	var out struct{}
	err := p.getJSON(context.Background(), "tok", "/team", &out)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected 503 in error, got %v", err)
	}
}

func TestGetJSON_CancelledContextNoNetwork(t *testing.T) {
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not have been hit with a cancelled ctx")
	})
	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out struct{}
	err := p.getJSON(ctx, "tok", "/team", &out)
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
}

func TestFetchListTasks_PaginatesUntilLastPage(t *testing.T) {
	// Two-page response: the first carries last_page=false, the
	// second last_page=true. Confirms our loop honours the API's
	// termination signal rather than relying on len(Tasks) only.
	var hits int32
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := atomic.AddInt32(&hits, 1)
		switch page {
		case 1:
			// Confirm first page asks for page=0
			if !strings.Contains(r.URL.RawQuery, "page=0") {
				t.Errorf("expected page=0 on first call; got %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{
				"tasks": [
					{"id":"t1","name":"a","status":{"status":"open","type":"open"}},
					{"id":"t2","name":"b","status":{"status":"open","type":"open"}}
				],
				"last_page": false
			}`))
		case 2:
			if !strings.Contains(r.URL.RawQuery, "page=1") {
				t.Errorf("expected page=1 on second call; got %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{
				"tasks": [
					{"id":"t3","name":"c","status":{"status":"closed","type":"closed"}}
				],
				"last_page": true
			}`))
		default:
			t.Errorf("unexpected page hit: %d", page)
		}
	})

	p := New()
	tasks, err := p.fetchListTasks(context.Background(), "tok", "list-1")
	if err != nil {
		t.Fatalf("fetchListTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks across 2 pages, got %d", len(tasks))
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("expected 2 server hits, got %d", atomic.LoadInt32(&hits))
	}
}

func TestFetchListTasks_StopsOnEmptyPage(t *testing.T) {
	// Defensive: a response with last_page=false but tasks=[] could
	// otherwise loop forever. The loop exits on either signal.
	var hits int32
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"tasks": [], "last_page": false}`))
	})

	p := New()
	tasks, err := p.fetchListTasks(context.Background(), "tok", "list-1")
	if err != nil {
		t.Fatalf("fetchListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected exactly 1 server hit (loop bail-out), got %d", atomic.LoadInt32(&hits))
	}
}

func TestFetchListTasks_IncludesArchivedAndSubtasks(t *testing.T) {
	// The query string must include include_subtasks=true and
	// include_closed=true; without these ClickUp silently omits both.
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.RawQuery
		if !strings.Contains(q, "include_subtasks=true") {
			t.Errorf("missing include_subtasks=true in query: %s", q)
		}
		if !strings.Contains(q, "include_closed=true") {
			t.Errorf("missing include_closed=true in query: %s", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks": [], "last_page": true}`))
	})
	p := New()
	if _, err := p.fetchListTasks(context.Background(), "tok", "list-1"); err != nil {
		t.Fatalf("fetchListTasks: %v", err)
	}
}

func TestFetchSpaces_PassesArchivedFlag(t *testing.T) {
	// The space + folder + folderless-list endpoints all need
	// archived=false; without it ClickUp returns archived spaces too,
	// which then look like dead lists with empty tasks.
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "archived=false") {
			t.Errorf("missing archived=false in query: %s", r.URL.RawQuery)
		}
		if r.URL.Path != "/team/ws-1/space" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"spaces": [{"id": "s1", "name": "Engineering"}]}`))
	})
	p := New()
	spaces, err := p.fetchSpaces(context.Background(), "tok", "ws-1")
	if err != nil {
		t.Fatalf("fetchSpaces: %v", err)
	}
	if len(spaces) != 1 || spaces[0].ID != "s1" {
		t.Fatalf("unexpected spaces: %+v", spaces)
	}
}

func TestFetchTaskComments_PaginatesUsingStartId(t *testing.T) {
	// ClickUp's /task/{id}/comment is paginated via start (oldest
	// timestamp) + start_id (oldest id) of the previous page. Validate
	// the cursor advances correctly across two pages.
	var hits int32
	withFakeClickUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := atomic.AddInt32(&hits, 1)
		switch page {
		case 1:
			// First page should NOT have a cursor.
			if r.URL.RawQuery != "" {
				t.Errorf("first page should not include cursor query, got %s", r.URL.RawQuery)
			}
			// Return exactly the page size (25) so the loop continues.
			body := strings.Builder{}
			body.WriteString(`{"comments": [`)
			for i := 0; i < 25; i++ {
				if i > 0 {
					body.WriteString(",")
				}
				body.WriteString(`{"id":"c-`)
				body.WriteString(strconvItoa(i))
				body.WriteString(`","comment_text":"hi","date":"170000000`)
				body.WriteString(strconvItoa(i % 10))
				body.WriteString(`000"}`)
			}
			body.WriteString(`]}`)
			_, _ = w.Write([]byte(body.String()))
		case 2:
			// Second page must carry start + start_id from the oldest
			// (last in newest-first ordering) seen on page 1, which
			// is c-24 (i=24).
			if !strings.Contains(r.URL.RawQuery, "start_id=c-24") {
				t.Errorf("expected start_id=c-24 on page 2, got %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"comments": [{"id":"c-25","comment_text":"final","date":"1700000005000"}]}`))
		default:
			t.Errorf("unexpected page %d", page)
		}
	})
	p := New()
	comments, err := p.fetchTaskComments(context.Background(), "tok", "task-1")
	if err != nil {
		t.Fatalf("fetchTaskComments: %v", err)
	}
	if len(comments) != 26 {
		t.Fatalf("expected 26 comments across two pages, got %d", len(comments))
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", atomic.LoadInt32(&hits))
	}
}

// strconvItoa is a tiny inline alternative to importing strconv just
// for the fake server. Adding strconv would inflate the import block
// for one helper line.
func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
