package business

import (
	"errors"
	"reflect"
	"testing"
)

func TestSanitizeEvents(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"no empty", []string{"a", "b"}, []string{"a", "b"}},
		{"with empty", []string{"a", "", "b"}, []string{"a", "b"}},
		{"only empty", []string{"", ""}, nil},
		{"with duplicates", []string{"a", "a", "b"}, []string{"a", "b"}},
		{"mixed", []string{"a", "", "a", "b", "", "b"}, []string{"a", "b"}},
		// A blank kind names nothing, and a padded one is the kind it pads.
		{"only blank", []string{" ", "\t\n"}, nil},
		{"padded", []string{" a ", "a", "b\t"}, []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeEvents(c.input)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("sanitizeEvents(%v) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

// An outgoing webhook is sent only the kinds of event it names, so one that
// names none, blank ones included, is refused in words an admin can act on
// (V-112). An incoming webhook is sent nothing whatever it names, so it keeps
// what it is given.
func TestAnOutgoingWebhookNamesAtLeastOneKind(t *testing.T) {
	for _, none := range [][]string{nil, {}, {""}, {" ", "\t"}} {
		got, err := eventsColumn("outgoing", none)
		var said ScopeError
		if got != nil || !errors.As(err, &said) || said.Error() != noKinds {
			t.Errorf("outgoing, events %q: %v, %v; want refused with %q", none, got, err, noKinds)
		}
		if got, err := eventsColumn("incoming", none); got != nil || err != nil {
			t.Errorf("incoming, events %q: %v, %v; want none, and no error", none, got, err)
		}
	}
	got, err := eventsColumn("outgoing", []string{" post.created ", "post.created", "task.created"})
	if err != nil || got == nil || *got != `["post.created","task.created"]` {
		t.Errorf("outgoing with kinds: %v, %v", got, err)
	}
}

// A webhook's messages always carry a name: its own, trimmed, or the
// fallback when that leaves nothing, so its text never reaches the bot's
// label-less Markdown path.
func TestAWebhooksMessagesAlwaysCarryAName(t *testing.T) {
	for _, c := range []struct{ name, fallback, want string }{
		{"Deploys", defaultBotName, "Deploys"},
		{"  Deploys  ", defaultBotName, "Deploys"},
		{"", defaultBotName, defaultBotName},
		{" \t\n", defaultBotName, defaultBotName},
		{"   ", "Alerts", "Alerts"},
	} {
		if got := botNameOf(c.name, c.fallback); got != c.want {
			t.Errorf("botNameOf(%q, %q) = %q, want %q", c.name, c.fallback, got, c.want)
		}
	}
}
