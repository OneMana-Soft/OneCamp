package helpers

import "testing"

func TestRowPlaceholdersNumberAcrossRows(t *testing.T) {
	for _, c := range []struct {
		rows, width int
		want        string
	}{
		{0, 3, ""},
		{2, 0, ""},
		{1, 1, "($1)"},
		{2, 3, "($1, $2, $3),($4, $5, $6)"},
	} {
		if got := RowPlaceholders(c.rows, c.width); got != c.want {
			t.Errorf("RowPlaceholders(%d, %d) = %q, want %q", c.rows, c.width, got, c.want)
		}
	}
}
