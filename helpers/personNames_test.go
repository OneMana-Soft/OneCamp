package helpers

import (
	"strings"
	"testing"
)

// The same people the web app's rule accepts (lib/validation/names.test.ts,
// "person"), and the names sign-up and identity providers produce that the
// profile editor used to refuse.
func TestAPersonsNameFollowsTheWebAppsRule(t *testing.T) {
	for _, ok := range []string{"Li", "José Álvarez", "O'Brien", "O’Brien", "Mary-Jane", "Dr. Smith", "अकाश", "priya.raman",
		"李雷", "Ada Lovelace-Byron of Ockham and Elsewhere", " Sam ", strings.Repeat("a", 60)} {
		if !IsValidPersonName(ok) {
			t.Errorf("%q was refused", ok)
		}
	}
	for _, bad := range []string{"", "   ", "sam_1a2b3", "priya@corp.com", "<b>", "a/b", strings.Repeat("a", 61)} {
		if IsValidPersonName(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// What an identity provider gives is made into a name the rule accepts.
func TestANameFromAProviderIsCleaned(t *testing.T) {
	cases := map[string]string{
		"José O'Brien":            "José O'Brien",
		"octo_cat":                "octo cat",
		"priya.raman@corp.test":   "priya.raman",
		`CORP\priya`:              "CORP priya",
		"  Sam   Rivera ":         "Sam Rivera",
		"<script>":                "script",
		"___":                     "",
		strings.Repeat("ab ", 30): strings.TrimSpace(strings.Repeat("ab ", 20)),
	}
	for in, want := range cases {
		if got := CleanPersonName(in); got != want {
			t.Errorf("CleanPersonName(%q) = %q, want %q", in, got, want)
		}
		if got := CleanPersonName(in); got != "" && !IsValidPersonName(got) {
			t.Errorf("CleanPersonName(%q) = %q, which the rule refuses", in, got)
		}
	}
}

// A handle comes from the name: lowercase, an apostrophe dropped, words
// joined by a hyphen, a directory's own separators kept; and from the address
// when the name gives too little.
func TestAHandleComesFromTheName(t *testing.T) {
	cases := []struct{ name, email, want string }{
		{"José O'Brien", "jo@example.test", "josé-obrien"},
		{"Priya Raman", "p@example.test", "priya-raman"},
		{"priya.raman", "p@example.test", "priya.raman"},
		{"Mary - Jane", "m@example.test", "mary-jane"},
		{"李雷", "lilei@example.test", "李雷"},
		{"Ω", "omega.one@example.test", "omega.one"},
		{"!!", "x@example.test", "member"},
		{strings.Repeat("abcdef ", 10), "a@example.test", "abcdef-abcdef-abcdef-abcdef-ab"},
	}
	for _, c := range cases {
		if got := HandleFromName(c.name, c.email); got != c.want {
			t.Errorf("HandleFromName(%q, %q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}
}

// Two people with one name get sam, sam-2, sam-3: never a random suffix in
// what they see, and never longer than a handle may be.
func TestAHandleThatIsTakenGetsANumber(t *testing.T) {
	if got := HandleCandidate("sam", 1); got != "sam" {
		t.Errorf("first try = %q", got)
	}
	if got := HandleCandidate("sam", 3); got != "sam-3" {
		t.Errorf("third try = %q", got)
	}
	long := strings.Repeat("a", HandleMaxRunes)
	if got := HandleCandidate(long, 12); len([]rune(got)) > HandleMaxRunes || !strings.HasSuffix(got, "-12") {
		t.Errorf("a long handle's 12th try = %q", got)
	}
}

// A handle someone chooses has its own rule, checked only when they change
// it; whatever HandleFromName makes passes it.
func TestAChosenHandleFollowsItsRule(t *testing.T) {
	for _, ok := range []string{"sam", "sam-2", "priya.raman", "jo_ann", "josé-obrien", "李雷", "a1"} {
		if !IsValidHandle(ok) {
			t.Errorf("%q was refused", ok)
		}
	}
	for _, bad := range []string{"", "s", "Sam", "-sam", ".sam", "sam smith", "sam@x", strings.Repeat("a", 31)} {
		if IsValidHandle(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
	if got := NormalizeHandle("  @Sam.Smith "); got != "sam.smith" {
		t.Errorf("NormalizeHandle = %q", got)
	}
	for _, name := range []string{"José O'Brien", "Priya Raman", "李雷", "!!", "Ω"} {
		if h := HandleFromName(name, "someone@example.test"); !IsValidHandle(h) {
			t.Errorf("the handle made from %q, %q, breaks the rule", name, h)
		}
	}
}

// A name is kept in NFC, so "José" typed with a separate accent is the same
// name as with é; it has at least one letter or number; and it has nothing
// that can't be seen: a format character (a zero-width space), a variation
// selector, or an accent with no letter under it. Two names that looked the
// same could differ by one of those.
func TestANameIsWhatItLooksLike(t *testing.T) {
	if got := NormalizePersonName(" José "); got != "José" {
		t.Errorf("NormalizePersonName = %q, want José in NFC", got)
	}
	for name, want := range map[string]bool{
		"José":    true,
		"Zoë":      true,
		"...":      false,
		"- . '":    false,
		"Ana​":     false, // zero-width space
		"Ana‍Bo":   false, // zero-width joiner
		"Ana️":     false, // variation selector
		"́Ana":     false, // an accent on nothing
		"Ana ́":    false,
		"Nguyễn": true, // two marks on one letter
	} {
		if got := IsValidPersonName(name); got != want {
			t.Errorf("IsValidPersonName(%q) = %v, want %v", name, got, want)
		}
	}
	for raw, want := range map[string]string{
		"Ana​Smith": "AnaSmith",
		"...":       "",
		"́Ana":      "Ana",
		"José":     "José",
	} {
		if got := CleanPersonName(raw); got != want {
			t.Errorf("CleanPersonName(%q) = %q, want %q", raw, got, want)
		}
	}
}

// @everyone, @here, @channel, @all and @admin mean groups of people in a
// mention, so no person's handle may be one; and a handle is kept in NFC.
func TestSomeHandlesAreReserved(t *testing.T) {
	for _, h := range []string{"everyone", "here", "channel", "all", "admin"} {
		if IsValidHandle(h) || !HandleIsReserved(h) {
			t.Errorf("@%s can be a person's handle", h)
		}
	}
	if !IsValidHandle("admin-2") || HandleIsReserved("administrator") {
		t.Error("a handle that only starts like a reserved one is refused")
	}
	if got := NormalizeHandle(" @José "); got != "josé" {
		t.Errorf("NormalizeHandle = %q", got)
	}
	if got := HandleFromName("José O'Brien", ""); got != "josé-obrien" {
		t.Errorf("HandleFromName = %q", got)
	}
	if IsValidHandle("ana​bo") || IsValidHandle("ana️") {
		t.Error("a handle with an unseen character is accepted")
	}
}
