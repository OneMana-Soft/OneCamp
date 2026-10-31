package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
)

func withRoomAddress(t *testing.T) *[]string {
	t.Helper()
	oAt, oEnter, oUnlock, oAsked := roomAt, enterRoom, unlockRoom, roomAskedFrom
	t.Cleanup(func() { roomAt, enterRoom, unlockRoom, roomAskedFrom = oAt, oEnter, oUnlock, oAsked })
	var surfaces []string
	roomAskedFrom = func(_ context.Context, surface, _ string, _ int) redisStore.RateLimitResult {
		surfaces = append(surfaces, surface)
		return redisStore.RateLimitResult{Allowed: true}
	}
	roomAt = func(context.Context, string, string) (string, *guestModel.GuestGrant, error) {
		t.Fatal("the address was read")
		return "", nil, nil
	}
	enterRoom = func(context.Context, string, string) (*guestModel.GuestGrant, error) {
		t.Fatal("the room was entered")
		return nil, nil
	}
	unlockRoom = func(context.Context, string, string, string) (string, time.Duration, error) {
		t.Fatal("a passcode was tried")
		return "", 0, nil
	}
	return &surfaces
}

// roomRoutes are the public room routes as router.go mounts them.
func roomRoutes() http.Handler {
	h := chi.NewRouter()
	h.Get("/public/r/{slug}", RoomAtAddress)
	h.Post("/public/r/{slug}/unlock", UnlockRoom)
	h.Post("/public/r/{slug}/join", JoinRoom)
	h.Get("/public/r/{slug}/knock/{ticket}", RoomKnockAnswer)
	h.Delete("/public/r/{slug}/knock/{ticket}", RoomKnockAnswer)
	return h
}

func askRoom(method, target, body string) (int, map[string]json.RawMessage, http.Header) {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.9:40000"
	w := httptest.NewRecorder()
	roomRoutes().ServeHTTP(w, r)
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Header()
}

func field(raw json.RawMessage, name string) string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	var s string
	_ = json.Unmarshal(m[name], &s)
	return s
}

// A room's address says live or locked and nothing more while locked; a dead
// one is the same 404 every dead link is, and a failed read a 503 the page
// retries. The address and the unlock it carries are the ones asked.
func TestARoomsAddressSaysOnlyWhatItMay(t *testing.T) {
	surfaces := withRoomAddress(t)
	var slug, unlock string
	answer := func(state string, err error) {
		roomAt = func(_ context.Context, s, k string) (string, *guestModel.GuestGrant, error) {
			slug, unlock = s, k
			return state, nil, err
		}
	}

	answer(guestBusiness.StateLocked, nil)
	code, out, _ := askRoom(http.MethodGet, "/public/r/maya-chen?k=tok", "")
	if code != http.StatusOK || field(out["data"], "state") != "locked" || strings.Contains(string(out["data"]), "title") || slug != "maya-chen" || unlock != "tok" {
		t.Errorf("a locked room: %d %s (%q %q)", code, out["data"], slug, unlock)
	}
	for _, state := range []string{guestBusiness.StateMissing, guestBusiness.StateGone} {
		answer(state, nil)
		if code, out, _ := askRoom(http.MethodGet, "/public/r/maya-chen", ""); code != http.StatusNotFound || string(out["available"]) != "false" {
			t.Errorf("%s: %d %v", state, code, out)
		}
	}
	answer("", errors.New("the database is away"))
	if code, _, _ := askRoom(http.MethodGet, "/public/r/maya-chen", ""); code != http.StatusServiceUnavailable {
		t.Errorf("a failed read: %d", code)
	}
	if len(*surfaces) != 4 || (*surfaces)[0] != "room" {
		t.Errorf("each read is counted as the room's: %v", *surfaces)
	}

	// Past the address's asks, nothing is read, and the 429 is every public
	// read's: the limiter's own wait, and its body (it said 60 and "code").
	roomAskedFrom = func(context.Context, string, string, int) redisStore.RateLimitResult {
		return redisStore.RateLimitResult{RetryAfter: 17 * time.Second}
	}
	roomAt = func(context.Context, string, string) (string, *guestModel.GuestGrant, error) {
		t.Fatal("read past the limit")
		return "", nil, nil
	}
	if code, out, h := askRoom(http.MethodGet, "/public/r/maya-chen", ""); code != http.StatusTooManyRequests || h.Get("Retry-After") != "17" || string(out["status"]) != `"rate_limited"` {
		t.Errorf("past the limit: %d, Retry-After %q, status %s", code, h.Get("Retry-After"), out["status"])
	}
}

// A passcode's unlock, a wrong one, and too many tries.
func TestARoomsPasscode(t *testing.T) {
	surfaces := withRoomAddress(t)
	var ip string
	try := func(token string, err error) {
		unlockRoom = func(_ context.Context, slug, passcode, from string) (string, time.Duration, error) {
			ip = from
			if slug != "maya-chen" || passcode != "4321" {
				t.Fatalf("tried %q at %q", passcode, slug)
			}
			return token, 12 * time.Hour, err
		}
	}
	try("opens", nil)
	code, out, _ := askRoom(http.MethodPost, "/public/r/maya-chen/unlock", `{"passcode":"4321"}`)
	var unlocked struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(out["data"], &unlocked); code != http.StatusOK || err != nil || unlocked.Token != "opens" || unlocked.ExpiresIn != 43200 || ip != "203.0.113.9" {
		t.Errorf("the right passcode: %d %s (from %q)", code, out["data"], ip)
	}
	for _, c := range []struct {
		err  error
		code int
		word string
	}{
		{callRoomBusiness.ErrWrongPasscode, http.StatusUnauthorized, "wrong_passcode"},
		{callRoomBusiness.ErrBusy, http.StatusTooManyRequests, "rate_limited"},
		{guestBusiness.ErrInvalidGrant, http.StatusNotFound, ""},
	} {
		try("", c.err)
		if code, out, _ := askRoom(http.MethodPost, "/public/r/maya-chen/unlock", `{"passcode":"4321"}`); code != c.code || codeOf(out) != c.word {
			t.Errorf("%v: %d %q", c.err, code, codeOf(out))
		}
	}
	if (*surfaces)[0] != "room-unlock" {
		t.Errorf("tries are counted as the room's unlock: %v", *surfaces)
	}
}

// Joining and knocking at a room with a passcode its page hasn't unlocked is
// asked for the passcode, before anything else; a dead address is the 404.
func TestARoomsDoorAsksForItsPasscode(t *testing.T) {
	surfaces := withRoomAddress(t)
	enterRoom = func(_ context.Context, slug, unlock string) (*guestModel.GuestGrant, error) {
		if unlock == "" {
			return nil, callRoomBusiness.ErrLocked
		}
		return nil, guestBusiness.ErrInvalidGrant
	}
	if code, out, _ := askRoom(http.MethodPost, "/public/r/maya-chen/join", `{"display_name":"Gia"}`); code != http.StatusUnauthorized || codeOf(out) != "locked" {
		t.Errorf("joining a locked room: %d %q", code, codeOf(out))
	}
	if code, out, _ := askRoom(http.MethodGet, "/public/r/maya-chen/knock/5b1f9a2e-3c4d-4e5f-8a9b-0c1d2e3f4a5b", ""); code != http.StatusUnauthorized || codeOf(out) != "locked" {
		t.Errorf("a knock on a locked room: %d %q", code, codeOf(out))
	}
	if code, out, _ := askRoom(http.MethodPost, "/public/r/maya-chen/join?k=stale", `{"display_name":"Gia"}`); code != http.StatusNotFound || string(out["available"]) != "false" {
		t.Errorf("a dead address: %d %v", code, out)
	}
	if code, _, _ := askRoom(http.MethodPost, "/public/r/maya-chen/join", `not json`); code != http.StatusBadRequest {
		t.Errorf("a join that can't be read: %d", code)
	}
	want := []string{"room-join", "room-knock", "room-join", "room-join"}
	if strings.Join(*surfaces, ",") != strings.Join(want, ",") {
		t.Errorf("asks counted as %v, want %v", *surfaces, want)
	}
}

func codeOf(out map[string]json.RawMessage) string {
	var c string
	_ = json.Unmarshal(out["code"], &c)
	return c
}
