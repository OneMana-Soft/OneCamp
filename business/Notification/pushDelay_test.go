package notification

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
)

// notify.push_delay (A4 H4) is how long a push waits while its person is in
// OneCamp: right away, or after 1, 5 or 10 minutes; a minute until they choose.

func TestThePushDelayIsAPreference(t *testing.T) {
	if PushDelayKey != "notify.push_delay" {
		t.Fatalf("key %q", PushDelayKey)
	}
	if !slices.Contains(preferencesBusiness.Keys(), PushDelayKey) {
		t.Fatalf("%s isn't registered", PushDelayKey)
	}
	for _, ok := range []string{"0", "1", "5", "10"} {
		if err := pushDelayProblem(json.RawMessage(ok)); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"2", "-1", "1.5", "60", `"5"`, "true", "{}", "[1]"} {
		err := pushDelayProblem(json.RawMessage(bad))
		var refused *preferencesBusiness.Refused
		if !errors.As(err, &refused) || refused.Msg == "" {
			t.Errorf("%s kept (%v); want it refused in words", bad, err)
		}
	}
}

func TestAPushDelayReadsAsMinutes(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": time.Minute, "null": time.Minute, "0": 0, "1": time.Minute, "5": 5 * time.Minute, "10": 10 * time.Minute, "7": time.Minute, `"x"`: time.Minute} {
		var v json.RawMessage
		if raw != "" {
			v = json.RawMessage(raw)
		}
		if got := pushDelayOf(v); got != want {
			t.Errorf("%q reads as %v, want %v", raw, got, want)
		}
	}
}

// The one sender splits in two on the same path (A4 H4): the desktop half is
// the notice alone, the push half the push alone, each past the same gate.
func TestEachHalfSendsItsOwnPart(t *testing.T) {
	reach := func(_ context.Context, ids []string) ([]string, []string, error) {
		return []string{tom}, []string{"tok-tom-phone"}, nil
	}
	r := recordMqtt(t)
	pushes := watchPushes(t)
	if err := deliverHalves(context.Background(), DesktopHalf, []string{tom}, chatPush("Lunch?"), "chat:g:m1", reach); err != nil {
		t.Fatal(err)
	}
	if got := r.notices(t, tom); len(got) != 1 || len(pushes()) != 0 {
		t.Fatalf("desktop half: notices %v, pushes %v; want the notice alone", got, pushes())
	}
	r = recordMqtt(t)
	if err := deliverHalves(context.Background(), PushHalf, []string{tom}, chatPush("Lunch?"), "chat:g:m1", reach); err != nil {
		t.Fatal(err)
	}
	if got := r.notices(t, tom); len(got) != 0 || len(pushes()) != 1 {
		t.Fatalf("push half: notices %v, pushes %v; want the push alone", got, pushes())
	}
}

// A waiting push keeps only what the push shows: its title, a short preview
// and the ids it opens; never the sender's name or icon, nor a long body.
func TestAWaitingPushKeepsOnlyWhatItShows(t *testing.T) {
	long := strings.Repeat("a", helpers.PushBodyMax+500)
	in := chatPush(long)
	in[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = "Maya in #launch"
	in[firebaseInit.FIREBASE_PUSH_DATA_ICON] = "https://files.example/avatar?sig=x"
	in[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = "maya"
	got := PushPreview(in)
	for _, k := range []string{firebaseInit.FIREBASE_PUSH_DATA_ICON, firebaseInit.FIREBASE_PUSH_DATA_USERNAME} {
		if _, kept := got[k]; kept {
			t.Errorf("kept %s", k)
		}
	}
	if got[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Maya in #launch" || len(got[firebaseInit.FIREBASE_PUSH_DATA_BODY]) > helpers.PushBodyMax+len("…") {
		t.Errorf("title %q, body %d bytes", got[firebaseInit.FIREBASE_PUSH_DATA_TITLE], len(got[firebaseInit.FIREBASE_PUSH_DATA_BODY]))
	}
	// The ids say where the push opens when it goes.
	for _, k := range []string{firebaseInit.FIREBASE_PUSH_DATA_TYPE, firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID, firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID} {
		if got[k] == "" || got[k] != in[k] {
			t.Errorf("%s %q, want the push's own %q", k, got[k], in[k])
		}
	}
}
