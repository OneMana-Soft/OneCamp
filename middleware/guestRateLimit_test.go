package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// A client approving a sprint's tasks can still comment, from the same
// address, and is told what happened when they do reach a cap.
func TestAGuestsWritesEachHaveTheirOwnGenerousLimit(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in the process

	send := func(kind GuestWrite, addr string) *httptest.ResponseRecorder {
		h := GuestRateLimit(kind)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/guest/project/tok/task/t1/review", nil)
		req.RemoteAddr = addr + ":4242"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for kind, max := range guestWriteCaps {
		floor := 100
		if kind == GuestJoin {
			floor = 20
		}
		if max < floor {
			t.Errorf("%s: %d in 15 minutes is not enough for a busy session", kind, max)
		}
	}

	const office = "198.51.100.9"
	for i := 0; i < guestWriteCaps[GuestApproval]; i++ {
		if rec := send(GuestApproval, office); rec.Code != http.StatusOK {
			t.Fatalf("approval %d: %d", i+1, rec.Code)
		}
	}
	rec := send(GuestApproval, office)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("one past the cap: %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "login") || !strings.Contains(body, "Wait a few minutes") {
		t.Errorf("a guest at the cap is told %s", body)
	}
	if rec := send(GuestComment, office); rec.Code != http.StatusOK {
		t.Errorf("approvals used up the comments: %d", rec.Code)
	}
	if rec := send(GuestApproval, "198.51.100.10"); rec.Code != http.StatusOK {
		t.Errorf("another address was limited: %d", rec.Code)
	}
}

// A leaked link used from many addresses is still limited, as the link, and
// another link isn't touched by it.
func TestAGuestLinkHasItsOwnLimitAcrossAddresses(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in the process
	saved := guestLinkCaps[GuestMessage]
	t.Cleanup(func() { guestLinkCaps[GuestMessage] = saved })
	guestLinkCaps[GuestMessage] = 4

	r := chi.NewRouter()
	r.With(GuestRateLimit(GuestMessage)).Post("/guest/channel/{token}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	send := func(token, addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/guest/channel/"+token, nil)
		req.RemoteAddr = addr + ":4242"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	for i := 1; i <= 4; i++ {
		if rec := send("leaked-link", fmt.Sprintf("203.0.113.%d", i)); rec.Code != http.StatusOK {
			t.Fatalf("message %d from its own address: %d", i, rec.Code)
		}
	}
	rec := send("leaked-link", "203.0.113.99")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "through this link") || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a fifth address on the same link: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send("another-link", "203.0.113.99"); rec.Code != http.StatusOK {
		t.Errorf("another link from the same address: %d", rec.Code)
	}
}

// Joining a meeting and opening a live doc or board count readers, not
// writes: a big guest meeting, or every guest's page reconnecting after the
// collaboration service blinked, never locks the link out. Each address is
// still held to its own cap.
func TestReadersThroughALinkAreLimitedOnlyByAddress(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in the process

	for _, c := range []struct {
		kind GuestWrite
		path string
	}{{GuestJoin, "/guest/meet/{token}/join"}, {GuestOpenLive, "/guest/collab/{token}"}} {
		r := chi.NewRouter()
		r.With(GuestRateLimit(c.kind)).Post(c.path, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		send := func(addr string) int {
			req := httptest.NewRequest(http.MethodPost, strings.Replace(c.path, "{token}", "big-meeting-"+string(c.kind), 1), nil)
			req.RemoteAddr = addr + ":4242"
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			return rec.Code
		}
		// Five hundred guests, from their own addresses, twice each.
		for i := 0; i < 1000; i++ {
			if code := send(fmt.Sprintf("10.%d.%d.1", i%500/250, i%250)); code != http.StatusOK {
				t.Fatalf("%s %d through one link: %d", c.kind, i+1, code)
			}
		}
		const one = "198.51.100.77"
		for i := 0; i < guestWriteCaps[c.kind]; i++ {
			send(one)
		}
		if code := send(one); code != http.StatusTooManyRequests {
			t.Errorf("%s: one address past its cap: %d", c.kind, code)
		}
	}
}
