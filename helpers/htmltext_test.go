package helpers

import (
	"testing"
	"unicode/utf8"
)

func TestHTMLToPlainText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain passthrough", "just text", "just text"},
		{
			"tiptap paragraph",
			`<p class="text-node">hello world</p>`,
			"hello world",
		},
		{
			"heading node",
			`<h3 class="heading-node">The history of computers</h3>`,
			"The history of computers",
		},
		{
			"block boundary inserts space",
			`<p class="text-node">uuuuppp</p><p class="text-node">next</p>`,
			"uuuuppp next",
		},
		{
			"entities unescaped",
			`<p>Tom &amp; Jerry &lt;3 &nbsp;done</p>`,
			"Tom & Jerry <3 done",
		},
		{
			// The exact shape seen in the activity feed / notifications:
			// numeric character references for apostrophe and quote.
			"numeric entities unescaped",
			`<p>Here&#39;s the &#34;summary&#34;</p>`,
			`Here's the "summary"`,
		},
		{
			"line break becomes space",
			`first<br/>second`,
			"first second",
		},
		{
			"list items don't run together",
			`<ul><li>one</li><li>two</li></ul>`,
			"one two",
		},
		{
			"collapses whitespace",
			"a\n\n   b\t c",
			"a b c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HTMLToPlainText(c.in); got != c.want {
				t.Errorf("HTMLToPlainText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 10, ""},
		{"under the limit", "short", 10, "short"},
		{"exactly at the limit", "abcde", 5, "abcde"},
		{"over the limit", "abcdefgh", 5, "abcde"},
		{"zero limit", "abc", 0, ""},
		{"negative limit", "abc", -1, ""},
		// The whole reason this exists: each of these characters is multiple bytes,
		// so a byte slice would cut one in half and produce invalid UTF-8.
		{"multi-byte counted as characters", "日本語のテキスト", 3, "日本語"},
		{"emoji not split", "👍👍👍👍", 2, "👍👍"},
		{"accents counted as characters", "café-society", 4, "café"},
		{"multi-byte under limit passes through", "日本語", 10, "日本語"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateRunes(c.in, c.max)
			if got != c.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunes(%q, %d) returned invalid UTF-8: %q", c.in, c.max, got)
			}
		})
	}
}

// TestTruncateRunesNeverSplitsACharacter is the property version of the cases above:
// whatever the cut point, the result must stay valid UTF-8 and stay within the
// limit. A byte-slicing implementation fails this at almost every offset.
func TestTruncateRunesNeverSplitsACharacter(t *testing.T) {
	// Deliberately mixes 1-, 2-, 3- and 4-byte characters so most byte offsets fall
	// inside a character.
	in := "aé日👍bñ漢字🎉cü"
	for limit := 0; limit <= utf8.RuneCountInString(in)+3; limit++ {
		got := TruncateRunes(in, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
		}
		if n := utf8.RuneCountInString(got); n > limit {
			t.Fatalf("limit %d produced %d characters: %q", limit, n, got)
		}
	}
}

func TestTruncateRunesWithSuffix(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		max    int
		suffix string
		want   string
	}{
		{"under the limit gets no suffix", "short", 10, "…", "short"},
		{"exactly at the limit gets no suffix", "abcde", 5, "…", "abcde"},
		{"over the limit gets the suffix", "abcdefgh", 5, "…", "abcde…"},
		{"empty stays empty", "", 5, "…", ""},
		{"zero limit", "abc", 0, "…", ""},
		{"multi-byte counted as characters", "日本語のテキスト", 3, "…", "日本語…"},
		// The bug this replaces: 3 characters of Japanese is 9 bytes, so a
		// byte-based guard would think truncation happened and append a suffix to
		// content that was never cut.
		{"multi-byte under limit is untouched", "日本語", 5, "…", "日本語"},
		{"emoji not split", "👍👍👍👍", 2, "...", "👍👍..."},
		{"suffix may be empty", "abcdefgh", 4, "", "abcd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateRunesWithSuffix(c.in, c.max, c.suffix)
			if got != c.want {
				t.Errorf("TruncateRunesWithSuffix(%q, %d, %q) = %q, want %q",
					c.in, c.max, c.suffix, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("returned invalid UTF-8: %q", got)
			}
		})
	}
}

// The suffix must never be appended to content that was not shortened, whatever
// the script. A byte-based guard fails this for every non-Latin input.
func TestTruncateRunesWithSuffixOnlyMarksRealTruncation(t *testing.T) {
	for _, s := range []string{"hello", "日本語", "café", "👍👍", "aé日👍"} {
		n := utf8.RuneCountInString(s)
		if got := TruncateRunesWithSuffix(s, n, "…"); got != s {
			t.Errorf("a string of exactly %d characters was marked as truncated:\n in: %q\nout: %q", n, s, got)
		}
		if got := TruncateRunesWithSuffix(s, n+1, "…"); got != s {
			t.Errorf("a string under the limit was marked as truncated:\n in: %q\nout: %q", s, got)
		}
	}
}
