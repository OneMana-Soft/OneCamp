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
