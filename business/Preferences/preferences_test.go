package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// What Set refuses, before anything is written.
func TestSetRefusesWhatItCantKeep(t *testing.T) {
	ctx, me := context.Background(), uuid.New()
	if err := Set(ctx, me, "no.such_key", json.RawMessage(`1`)); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("an unknown key: %v", err)
	}
	var refused *Refused
	for _, c := range []struct {
		key, value, says string
	}{
		{"zone", `"Mars/Olympus"`, "That isn't a time zone this server knows."},
		{"zone", `"Local"`, "That isn't a time zone this server knows."},
		{"zone", `42`, "Pick a time zone."},
		{"week_start", `7`, "A week starts on a day from Sunday (0) to Saturday (6)."},
		{"clock", `"on"`, `The clock is "auto", "12" or "24".`},
		{"clock", `{not json`, "That isn't a value this preference can keep."},
		{"zone", `"` + strings.Repeat("a", MaxValueBytes) + `"`, "Keep it under 4 KB."},
	} {
		if err := Set(ctx, me, c.key, json.RawMessage(c.value)); !errors.As(err, &refused) || refused.Msg != c.says {
			t.Errorf("%s = %.20s: %v", c.key, c.value, err)
		}
	}
}

// Every person-wide key is there, with its default, and the defaults are values.
func TestThePersonWideKeysAndTheirDefaults(t *testing.T) {
	want := map[string]string{"zone": "null", "week_start": "1", "clock": `"auto"`}
	for name, def := range want {
		k, ok := keyOf(name)
		if !ok || string(k.Default) != def {
			t.Errorf("%s: %v %s", name, ok, k.Default)
		}
	}
}

// Equal values are equal however they're written; a default set is a clear.
func TestSameJSON(t *testing.T) {
	if !sameJSON(json.RawMessage(`{"a": 1, "b": [1,2]}`), json.RawMessage(`{"b":[1,2],"a":1}`)) {
		t.Error("the same object, differently spaced and ordered")
	}
	if sameJSON(json.RawMessage(`"12"`), json.RawMessage(`12`)) {
		t.Error("a string isn't a number")
	}
}

// A key is declared once.
func TestAKeyIsRegisteredOnce(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a second zone was registered")
		}
	}()
	Register("zone", Key{Default: json.RawMessage("null")})
}
