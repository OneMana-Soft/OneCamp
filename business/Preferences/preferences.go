// Package business (Preferences) is Person Preferences (design #37, built by
// calendar A3a to chat A7's spec, agreed with CHAT-LEAD on 18 Oct): what a
// person chooses for themselves, kept once, for every surface. Each key is
// declared by the package that owns it (Register), with its default and its
// check; an unknown key is refused. Person-wide keys are unprefixed (zone,
// week_start, clock, and chat's skin_tone, sounds…); a surface's own are
// prefixed with it (calendar.working_hours, chat.density…). Only the person
// reads and writes their own (a token acts as its person); a set is told to
// their other devices on their own activity topic.
package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	models "github.com/akashc777/OneCamp/models/postgres/Preference"
	"github.com/google/uuid"
)

// MaxValueBytes caps one value's JSON, whatever its key's own check allows.
const MaxValueBytes = 4096

// Key is a preference as its owner declares it.
type Key struct {
	// Default is the value the key reads as until the person sets one
	// (JSON; "null" for none).
	Default json.RawMessage
	// Check says why a value can't be kept, in words for the person, or nil.
	Check func(value json.RawMessage) error
}

var (
	mu   sync.RWMutex
	keys = map[string]Key{}
)

// Register declares a key, once, from its owner's package (an init).
func Register(name string, k Key) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := keys[name]; dup {
		panic("preferences: " + name + " registered twice")
	}
	if !json.Valid(k.Default) {
		panic("preferences: " + name + "'s default isn't JSON")
	}
	keys[name] = k
}

// Keys is every registered key, in order.
func Keys() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keyOf(name string) (Key, bool) {
	mu.RLock()
	defer mu.RUnlock()
	k, ok := keys[name]
	return k, ok
}

// Refused is a value that can't be kept; its text is written for the person.
type Refused struct{ Msg string }

func (r *Refused) Error() string { return r.Msg }

// ErrUnknownKey is a key nobody registered.
var ErrUnknownKey = errors.New("preferences: no such preference")

// Get is every registered key's value for this person: what they set, or its default.
func Get(ctx context.Context, user uuid.UUID) (map[string]json.RawMessage, error) {
	set, err := models.All(ctx, user)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, name := range Keys() {
		k, _ := keyOf(name)
		if v, ok := set[name]; ok {
			out[name] = v
		} else {
			out[name] = k.Default
		}
	}
	return out, nil
}

// OfPeople is one key's value for each of these people: what they set, or
// nothing for those who didn't (their default applies), in one read.
func OfPeople(ctx context.Context, people []uuid.UUID, name string) (map[uuid.UUID]json.RawMessage, error) {
	if _, ok := keyOf(name); !ok {
		return nil, ErrUnknownKey
	}
	return models.OfPeople(ctx, name, people)
}

// Set keeps a value for this person, once its key's check passes; a value
// equal to the default (or null) clears it. Their other devices are told.
func Set(ctx context.Context, user uuid.UUID, name string, value json.RawMessage) error {
	k, ok := keyOf(name)
	if !ok {
		return ErrUnknownKey
	}
	if len(value) == 0 || !json.Valid(value) {
		return &Refused{"That isn't a value this preference can keep."}
	}
	if len(value) > MaxValueBytes {
		return &Refused{fmt.Sprintf("Keep it under %d KB.", MaxValueBytes/1024)}
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, value)
	value = compact.Bytes()
	clear := bytes.Equal(value, []byte("null")) || sameJSON(value, k.Default)
	if !clear && k.Check != nil {
		if err := k.Check(value); err != nil {
			return err
		}
	}
	var err error
	if clear {
		err = models.Clear(ctx, user, name)
		value = k.Default
	} else {
		err = models.Put(ctx, user, name, value)
	}
	if err != nil {
		return err
	}
	// Their other devices follow, on their own activity topic only.
	mqttBusiness.PublishMessageToUser(user.String(), mqttStruct.MESSAGE_PREFERENCE, map[string]any{"key": name, "value": value})
	return nil
}

// sameJSON is whether two JSON values are equal as values. Pure.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}
