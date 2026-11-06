package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// asMember sends n requests of method from addr through UserRateLimit(kind,
// max), signed in as member (uuid.Nil: no one signed in), and returns their
// status codes.
func asMember(t *testing.T, kind string, max int, member uuid.UUID, addr, method string, n int) []int {
	t.Helper()
	h := UserRateLimit(kind, max, "slow down")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
