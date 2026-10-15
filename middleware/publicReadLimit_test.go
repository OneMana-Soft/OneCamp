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
