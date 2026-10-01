package helpers

import "testing"

func TestReleaseLine(t *testing.T) {
	for in, want := range map[string]string{"v2.33.0": "v2", " v1.19.2 ": "v1", "": "", "main": "", "v2.33": "", "2.33.0": ""} {
		if got := ReleaseLine(in); got != want {
			t.Errorf("ReleaseLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareReleaseTags(t *testing.T) {
	cases := []struct {
		a, b string
		sign int
	}{
		{"v2.10.0", "v2.9.9", 1}, // numeric, not lexical
		{"v2.32.1", "v2.32.1", 0},
		{"v2.32.0", "v2.32.1", -1},
		{"v3.0.0", "v2.99.99", 1},
		{"", "v2.0.0", 0},
		{"dev", "v2.0.0", 0},
	}
	for _, c := range cases {
		got := CompareReleaseTags(c.a, c.b)
		if (got > 0) != (c.sign > 0) || (got < 0) != (c.sign < 0) {
			t.Errorf("CompareReleaseTags(%q, %q) = %d, want sign %d", c.a, c.b, got, c.sign)
		}
	}
}
