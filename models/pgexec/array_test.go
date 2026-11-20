package pgexec

import (
	"database/sql/driver"
	"testing"
)

// TestArrayIsOneArrayArgument: a list goes as one Postgres array literal, and
// a nil list as NULL, which text such as `$2::uuid[] IS NULL` reads as "no
// list given" (domain/UserProjectNotification LevelsInProject).
func TestArrayIsOneArrayArgument(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want driver.Value
	}{
		{nil, nil},
		{[]string{}, "{}"},
		{[]string{"a", "b"}, `{"a","b"}`},
	} {
		v, err := Array(c.in).(driver.Valuer).Value()
		if err != nil || v != c.want {
			t.Errorf("Array(%#v) = %#v, %v; want %#v", c.in, v, err, c.want)
		}
	}
}
