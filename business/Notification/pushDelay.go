package notification

// The phone waits while you're in OneCamp (A4 H4): notify.push_delay is how
// long a chat push waits while its person is in OneCamp (Presence Here: a
// device of theirs connected, not set offline), so it isn't sent when they
// read the message first (business/Notify). Right away (0), or after 1, 5 or
// 10 minutes; a minute until they choose. It is the person's own (Person
// Preferences: only they read and set it).

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"time"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	"github.com/google/uuid"
)

// PushDelayKey is the preference: a JSON number of minutes.
const PushDelayKey = "notify.push_delay"

// pushDelayMinutes are the delays a person may choose.
var pushDelayMinutes = []int{0, 1, 5, 10}

// defaultPushDelay is the delay of someone who chose none.
const defaultPushDelay = time.Minute

func init() {
	preferencesBusiness.Register(PushDelayKey, preferencesBusiness.Key{
		Default: json.RawMessage("1"),
		Check:   pushDelayProblem,
	})
}

// pushDelayProblem says why value can't be kept: it must be 0, 1, 5 or 10.
func pushDelayProblem(value json.RawMessage) error {
	if _, ok := pushDelayMinutesOf(value); !ok {
		return &preferencesBusiness.Refused{Msg: "Choose right away, or after 1, 5 or 10 minutes."}
	}
	return nil
}

// pushDelayMinutesOf is value's minutes, when it is one of the delays a
// person may choose: a whole number (never "5", 1.5 or 1.0). Pure.
func pushDelayMinutesOf(value json.RawMessage) (int, bool) {
	var n int
	if err := json.Unmarshal(value, &n); err != nil || !slices.Contains(pushDelayMinutes, n) || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return 0, false
	}
	return n, true
}

// pushDelayOf is the delay value keeps: its minutes, or a minute when it is
// none of the choices (or unset). Pure.
func pushDelayOf(value json.RawMessage) time.Duration {
	n, ok := pushDelayMinutesOf(value)
	if !ok {
		return defaultPushDelay
	}
	return time.Duration(n) * time.Minute
}

// PushDelays is each of ids' push delay (keyed as given), in one read; ids
// that aren't user ids are left out.
func PushDelays(ctx context.Context, ids []string) (map[string]time.Duration, error) {
	people := make([]uuid.UUID, 0, len(ids))
	byID := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil {
			people = append(people, u)
			byID[u] = id
		}
	}
	out := make(map[string]time.Duration, len(people))
	if len(people) == 0 {
		return out, nil
	}
	set, err := preferencesBusiness.OfPeople(ctx, people, PushDelayKey)
	if err != nil {
		return nil, err
	}
	for _, u := range people {
		out[byID[u]] = pushDelayOf(set[u])
	}
	return out, nil
}
