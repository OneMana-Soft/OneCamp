package dgraphquery

import (
	"errors"
	"strings"
	"testing"
)

func TestEscapeRegexLiteral(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"hello world", "hello world"},
		{"a.b", `a\.b`},
		{"foo/bar", `foo\/bar`},
		{"a+b", `a\+b`},
		{"(group)", `\(group\)`},
		{"[class]", `\[class\]`},
		{"a|b", `a\|b`},
		{`back\slash`, `back\\slash`},
		{"résumé", "résumé"},
		{"日本語", "日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := EscapeRegexLiteral(tc.in); got != tc.want {
				t.Errorf("EscapeRegexLiteral(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSearchTerm_Valid(t *testing.T) {
	got, err := SanitizeSearchTerm("hello world")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestSanitizeSearchTerm_RejectInjection(t *testing.T) {
	got, err := SanitizeSearchTerm(`general/i)) OR has(admin)`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// All special chars escaped — no way to break out of the regex.
	if !strings.Contains(got, `\/`) || !strings.Contains(got, `\)`) {
		t.Errorf("expected escaping of / and ); got %q", got)
	}
}

func TestSanitizeSearchTerm_StripControlChars(t *testing.T) {
	got, err := SanitizeSearchTerm("hello\x00world\x01\u200b")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "helloworld" {
		t.Errorf("control chars not stripped; got %q", got)
	}
}

func TestSanitizeSearchTerm_EmptyAfterStrip(t *testing.T) {
	_, err := SanitizeSearchTerm("\x00\x01")
	if !errors.Is(err, ErrEmpty) {
		t.Errorf("want ErrEmpty, got %v", err)
	}
}

func TestSanitizeSearchTerm_TooLong(t *testing.T) {
	_, err := SanitizeSearchTerm(strings.Repeat("a", MaxSearchTermLen+1))
	if !errors.Is(err, ErrTooLong) {
		t.Errorf("want ErrTooLong, got %v", err)
	}
}

func TestIsAllowedColumnName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"task_status", true},
		{"task_priority", true},
		{"_under", true},
		{"a", true},
		{"", false},
		{"1bad", false},
		{"has space", false},
		{"a-b", false},
		{"a;b", false},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("a", 64), true},
		{"task_name'", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := IsAllowedColumnName(tc.in); got != tc.want {
				t.Errorf("IsAllowedColumnName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestOnlyUIDsGoIntoAQuerysUIDList(t *testing.T) {
	if !AllUIDs([]string{"0x1", "0xAbC123", "0x7fffffffffffffff"}) {
		t.Error("real uids were refused")
	}
	for _, bad := range [][]string{
		{"0x1) OR has(user_email_id"},
		{"0x1", "1"},
		{""},
		{"0x"},
		{"0x1 "},
		{"0x12345678901234567"},
	} {
		if AllUIDs(bad) {
			t.Errorf("%q was taken as uids", bad)
		}
	}
}

// A cleaned term is not escaped, so a caller that escapes it again, or also
// matches it in Postgres, gets the text as typed.
func TestCleanSearchTermDoesNotEscape(t *testing.T) {
	got, err := CleanSearchTerm("  priya.raman​ ")
	if err != nil || got != "priya.raman" {
		t.Fatalf("CleanSearchTerm = %q, %v", got, err)
	}
	if safe, _ := SanitizeSearchTerm("priya.raman"); safe != `priya\.raman` {
		t.Fatalf("SanitizeSearchTerm = %q, still escaped once", safe)
	}
}
