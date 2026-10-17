package business

import (
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
