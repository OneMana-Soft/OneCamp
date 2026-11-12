package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// asMember sends n requests of method from addr through UserRateLimit(kind,
// max), signed in as member (uuid.Nil: no one signed in), and returns their
// status codes.
func asMember(t *testing.T, kind string, max int, member uuid.UUID, addr, method string, n int) []int {
	t.Helper()
	h := UserRateLimit(kind, max, "slow down", nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	codes := make([]int, n)
	for i := range codes {
		req := httptest.NewRequest(method, "/board/recordExport", nil)
		req.RemoteAddr = addr + ":4242"
		if member != uuid.Nil {
			var who userModels.UserInfo
			who.UserPostgresInfo.Id = member
			req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, who))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes[i] = rec.Code
		if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: a 429 with no Retry-After", kind)
		}
	}
	return codes
}

// An export's tell is limited by who asks, not by where from: two members
// behind one office's address each have their own budget, so one busy member
// never costs another's exports their audit rows (V-363).
func TestAMembersWritesAreLimitedByTheMemberNotTheAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as in an outage

	const office = "198.51.100.20"
	kind := "test-user-export-" + uuid.NewString()
	ana, ben := uuid.New(), uuid.New()

	if got, want := asMember(t, kind, 2, ana, office, http.MethodPost, 3), []int{200, 200, 429}; !slices.Equal(got, want) {
		t.Errorf("ana: codes %v, want %v", got, want)
	}
	if got, want := asMember(t, kind, 2, ben, office, http.MethodPost, 2), []int{200, 200}; !slices.Equal(got, want) {
		t.Errorf("ben, behind ana's address: codes %v, want %v (ana's budget isn't his)", got, want)
	}
	// Ana from another address is still Ana.
	if got, want := asMember(t, kind, 2, ana, "203.0.113.9", http.MethodPost, 1), []int{429}; !slices.Equal(got, want) {
		t.Errorf("ana elsewhere: codes %v, want %v", got, want)
	}
	// Another surface has its own budget.
	if got, want := asMember(t, kind+"-other", 2, ana, office, http.MethodPost, 1), []int{200}; !slices.Equal(got, want) {
		t.Errorf("ana on another surface: codes %v, want %v", got, want)
	}
}

// Reads and preflights pass uncounted, and a request no one signed in to is
// counted by its address.
func TestAMembersLimitPassesReadsAndCountsTheSignedOutByAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil

	kind := "test-user-reads-" + uuid.NewString()
	ana := uuid.New()
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if got := asMember(t, kind, 1, ana, "198.51.100.30", m, 3); !slices.Equal(got, []int{200, 200, 200}) {
			t.Errorf("%s: codes %v, want every one through", m, got)
		}
	}
	if got, want := asMember(t, kind, 1, ana, "198.51.100.30", http.MethodPost, 2), []int{200, 429}; !slices.Equal(got, want) {
		t.Errorf("ana's writes after her reads: codes %v, want %v (reads aren't counted)", got, want)
	}

	if got, want := asMember(t, kind, 1, uuid.Nil, "198.51.100.31", http.MethodPost, 2), []int{200, 429}; !slices.Equal(got, want) {
		t.Errorf("signed out: codes %v, want %v", got, want)
	}
	if got, want := asMember(t, kind, 1, uuid.Nil, "198.51.100.32", http.MethodPost, 1), []int{200}; !slices.Equal(got, want) {
		t.Errorf("signed out, another address: codes %v, want %v", got, want)
	}
}

// heldOnce tells the first refusal of a window and no other: by Redis's count
// (one across every server) when Redis counted the request, else by a note
// this process keeps until the window ends (miro-44 (a)).
func TestHeldOnceTellsOnlyTheFirstRefusalOfAWindow(t *testing.T) {
	h := &heldOnce{until: map[string]time.Time{}}
	now := time.Now()
	counted := func(count int64) redisStore.RateLimitResult {
		return redisStore.RateLimitResult{Count: count, Limit: 60, RetryAfter: time.Minute}
	}
	for _, c := range []struct {
		count int64
		want  bool
	}{{61, true}, {62, false}, {200, false}} {
		if got := h.first("member:ana", counted(c.count), now); got != c.want {
			t.Errorf("Redis counted %d of 60: first = %v, want %v", c.count, got, c.want)
		}
	}
	if len(h.until) != 0 {
		t.Errorf("Redis's count kept %d notes in this process, want none", len(h.until))
	}

	local := redisStore.RateLimitResult{Limit: 60, RetryAfter: time.Minute, Unchecked: true}
	steps := []struct {
		who  string
		at   time.Duration
		want bool
	}{
		{"member:ana", 0, true},                 // her first refusal in this process's window
		{"member:ana", 30 * time.Second, false}, // pressing on in the same window
		{"member:ben", 30 * time.Second, true},  // another member's window is his own
		{"member:ana", time.Minute, true},       // the window has ended: a new one, a new row
		{"member:ana", 90 * time.Second, false}, // and once in it
	}
	for _, s := range steps {
		if got := h.first(s.who, local, now.Add(s.at)); got != s.want {
			t.Errorf("%s at +%v, counted here: first = %v, want %v", s.who, s.at, got, s.want)
		}
	}
	// Notes past their end are dropped as new ones are written: at +100s
	// Ben's (to +90s) has ended and Ana's (to +2m) hasn't.
	h.first("member:cy", local, now.Add(100*time.Second))
	if _, kept := h.until["member:ben"]; kept || len(h.until) != 2 {
		t.Errorf("notes after the sweep: %v, want ana's and cy's only", h.until)
	}
}

// UserRateLimit tells held once a window for each member, with the
// request it holds back (the member on its context); reads and the requests
// let through tell nothing.
func TestAHeldWriteIsToldOnceAWindowForEachMember(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as in an outage

	var told []uuid.UUID
	kind := "test-user-held-" + uuid.NewString()
	h := UserRateLimit(kind, 2, "slow down", func(r *http.Request) {
		u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
		told = append(told, u.UserPostgresInfo.Id)
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	send := func(member uuid.UUID, method string, n int) []int {
		codes := make([]int, n)
		for i := range codes {
			var who userModels.UserInfo
			who.UserPostgresInfo.Id = member
			req := httptest.NewRequest(method, "/board/recordExport", nil)
			req.RemoteAddr = "198.51.100.40:4242"
			req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, who))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}
		return codes
	}
	ana, ben := uuid.New(), uuid.New()
	if got, want := send(ana, http.MethodPost, 5), []int{200, 200, 429, 429, 429}; !slices.Equal(got, want) {
		t.Errorf("ana: codes %v, want %v", got, want)
	}
	if got, want := send(ana, http.MethodGet, 2), []int{200, 200}; !slices.Equal(got, want) {
		t.Errorf("ana's reads: codes %v, want %v", got, want)
	}
	if got, want := send(ben, http.MethodPost, 4), []int{200, 200, 429, 429}; !slices.Equal(got, want) {
		t.Errorf("ben, behind ana's address: codes %v, want %v", got, want)
	}
	if want := []uuid.UUID{ana, ben}; !slices.Equal(told, want) {
		t.Errorf("held was told for %v, want once for ana then once for ben %v", told, want)
	}
}
