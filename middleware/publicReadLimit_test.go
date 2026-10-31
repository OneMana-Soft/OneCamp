package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// bookingPages is GET /public/book/{slug} behind its limit, as the router
// mounts it, answering 200 when let through.
func bookingPages() http.Handler {
	r := chi.NewRouter()
	r.With(BookingPageReadLimit()).Get("/public/book/{slug}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return r
}

func read(h http.Handler, path, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = addr + ":4242"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// CAL-BUG-14: GETs of a booking page were never counted, so a script could
// make the server read its owner's calendars, Google's with the owner's
// token, as fast as it liked. Sixty a minute from one address now, then 429.
func TestABookingPageTurnsAwayTheSixtyFirstReadFromOneAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as when Redis is down

	h := bookingPages()
	for i := 1; i <= 60; i++ {
		if rec := read(h, fmt.Sprintf("/public/book/limit-one?from=2026-10-%02dT00:00:00Z", 1+i%28), "198.51.100.61"); rec.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i, rec.Code)
		}
	}
	rec := read(h, "/public/book/limit-one", "198.51.100.61")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the 61st read: %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["msg"] != publicReadAddressMsg || body["status"] != "rate_limited" {
		t.Errorf("answer %s: %v", rec.Body.String(), err)
	}
	// Someone else, and the same address on the guest's own link, are not held.
	if rec := read(h, "/public/book/limit-one", "198.51.100.62"); rec.Code != http.StatusOK {
		t.Errorf("another address: %d", rec.Code)
	}
	if rec := read(h, "/public/book/limit-other", "198.51.100.62"); rec.Code != http.StatusOK {
		t.Errorf("another page: %d", rec.Code)
	}
}

// The page's own bucket isn't counted here: it counted answers served from
// the cache too, for every visitor together, so one host could hold a booking
// page at 429 for everyone. It is counted where an answer is worked out
// afresh (business/Calendar).
func TestManyAddressesAreNotHeldBackByThePageHere(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil

	h := bookingPages()
	for i := 0; i < 11*bookingPageReadsPerAddress; i++ {
		addr := fmt.Sprintf("10.%d.%d.%d", 62+i/65536, i/256%256, i%256)
		if rec := read(h, "/public/book/flood-page", addr); rec.Code != http.StatusOK {
			t.Fatalf("read %d from %s: %d", i, addr, rec.Code)
		}
	}
}

// An IPv6 host is handed a whole /64: counted address by address, it could
// be a fresh address for every read. It is counted by its /64.
func TestAnIPv6HostIsOneAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil

	h := bookingPages()
	for i := 1; i <= bookingPageReadsPerAddress; i++ {
		if rec := read(h, "/public/book/v6-page", fmt.Sprintf("[2001:db8:61:1::%x]", i)); rec.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i, rec.Code)
		}
	}
	if rec := read(h, "/public/book/v6-page", "[2001:db8:61:1:ffff::1]"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("read %d from the same /64: %d, want 429", bookingPageReadsPerAddress+1, rec.Code)
	}
	if rec := read(h, "/public/book/v6-page", "[2001:db8:61:2::1]"); rec.Code != http.StatusOK {
		t.Errorf("another /64: %d", rec.Code)
	}
}

// Audit A3 (22 Oct 2026): IPRateLimit, and LoginRateLimit through it, let
// every GET through uncounted, so a guest link's reads behind them (a
// meeting, a table, a channel, a project; the "guest-channel-read" bucket was
// named for them) had no limit at all. Reads now count apart from writes, at
// a link page's budget a minute, which a client's team on one office
// connection fits in.
func TestAGuestLinksReadsAreCountedPerAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as when Redis is down

	r := chi.NewRouter()
	r.With(LoginRateLimit("guest-channel-read")).Get("/guest/channel/{token}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// 25 people, each with the channel and a thread open, both polling every
	// 5 s, for a minute.
	const team, readsAPerson = 25, 24
	for i := 1; i <= team*readsAPerson; i++ {
		if rec := read(r, "/guest/channel/tok-flood", "198.51.100.71"); rec.Code != http.StatusOK {
			t.Fatalf("read %d of a team's %d: %d", i, team*readsAPerson, rec.Code)
		}
	}
	for i := team*readsAPerson + 1; i <= linkReadsPerAddress; i++ {
		if rec := read(r, "/guest/channel/tok-flood", "198.51.100.71"); rec.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i, rec.Code)
		}
	}
	rec := read(r, "/guest/channel/tok-flood", "198.51.100.71")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("read %d from one address: %d, Retry-After %q", linkReadsPerAddress+1, rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := read(r, "/guest/channel/tok-flood", "198.51.100.72"); rec.Code != http.StatusOK {
		t.Errorf("another address: %d", rec.Code)
	}
}

// GET /auth/token is asked again by each open doc or board tab about every
// five minutes, and an office's tabs share one address: 300 of them coming
// back in the same minute (after a deploy, at nine) all get their token, and
// a flood past the budget is turned away.
func TestAnOfficesTabsAllGetTheirCollabToken(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil

	r := chi.NewRouter()
	r.With(CollabTokenReadLimit()).Get("/auth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	const officeTabs = 300
	for i := 1; i <= officeTabs; i++ {
		if rec := read(r, "/auth/token", "198.51.100.74"); rec.Code != http.StatusOK {
			t.Fatalf("tab %d of an office's %d: %d", i, officeTabs, rec.Code)
		}
	}
	for i := officeTabs + 1; i <= collabTokenReadsPerAddress; i++ {
		if rec := read(r, "/auth/token", "198.51.100.74"); rec.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i, rec.Code)
		}
	}
	rec := read(r, "/auth/token", "198.51.100.74")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("read %d from one address: %d, Retry-After %q", collabTokenReadsPerAddress+1, rec.Code, rec.Header().Get("Retry-After"))
	}
}

// A guest's pictures (a doc's, a board's) had no limit of any kind: many to a
// page, so a file's budget, but a budget.
func TestAGuestsFilesAreCountedPerAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil

	r := chi.NewRouter()
	r.With(FileReadLimit("guest-doc")).Get("/guest/doc-attachment/{token}/{obj_uuid}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for i := 1; i <= fileReadsPerAddress; i++ {
		if rec := read(r, fmt.Sprintf("/guest/doc-attachment/tok/obj-%d", i), "198.51.100.73"); rec.Code != http.StatusOK {
			t.Fatalf("file %d: %d", i, rec.Code)
		}
	}
	if rec := read(r, "/guest/doc-attachment/tok/obj-0", "198.51.100.73"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("file %d from one address: %d", fileReadsPerAddress+1, rec.Code)
	}
}

// CODE-REVIEWER (CALLS-3b): a guest's read of the call's chat was the one
// guest GET nothing counted. 120 a minute from one address, ten guests'
// polls, then 429.
func TestACallChatTurnsAwayTheHundredAndTwentyFirstReadFromOneAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as when Redis is down

	r := chi.NewRouter()
	r.With(CallThreadReadLimit()).Get("/guest/calls/thread", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for i := 1; i <= 120; i++ {
		if rec := read(r, "/guest/calls/thread?since=x", "198.51.100.71"); rec.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i, rec.Code)
		}
	}
	if rec := read(r, "/guest/calls/thread", "198.51.100.71"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the 121st read: %d, want 429", rec.Code)
	}
	if rec := read(r, "/guest/calls/thread", "198.51.100.72"); rec.Code != http.StatusOK {
		t.Errorf("another address: %d", rec.Code)
	}
}
