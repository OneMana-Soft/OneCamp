package business

import (
	"reflect"
	"testing"
)

func TestSplitArgsReadsCurlyQuotes(t *testing.T) {
	cases := map[string][]string{
		`"Lunch?" "Pizza" "Sushi"`:             {"Lunch?", "Pizza", "Sushi"},
		"“Lunch today?” “Pizza place” “Sushi”": {"Lunch today?", "Pizza place", "Sushi"},
		"„Wo?“ „Hier“":                         {"Wo?", "Hier"},
		`remind me tomorrow`:                   {"remind", "me", "tomorrow"},
		"“It’s Bob’s” x":                       {"It’s Bob’s", "x"},
	}
	for in, want := range cases {
		if got := splitArgs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
