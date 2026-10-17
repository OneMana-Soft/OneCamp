package business

import (
	"strings"
	"testing"
)

// A configured name becomes one tidy line a guest page can show, or "" for
// none so the web app keeps its own build's name.
func TestCleanWorkspaceName(t *testing.T) {
	cases := map[string]struct{ raw, want string }{
		"nothing":                  {"", ""},
		"blank":                    {"   \t ", ""},
		"as written":               {"Acme Studio", "Acme Studio"},
		"padded":                   {"  Acme Studio \n", "Acme Studio"},
		"spaces between words":     {"Acme \t  Studio", "Acme Studio"},
		"a newline inside":         {"Acme\nStudio", "Acme Studio"},
		"control characters":       {"Acme\x00\x07\x1b Studio", "Acme Studio"},
		"accents and scripts kept": {"Café Ōsaka 東京", "Café Ōsaka 東京"},
	}
	for name, c := range cases {
		if got := cleanWorkspaceName(c.raw); got != c.want {
			t.Errorf("%s: cleanWorkspaceName(%q) = %q, want %q", name, c.raw, got, c.want)
		}
	}
}

// The name is ORG_NAME's, read each time (an operator who sets it and
// restarts sees it at once), and "" while it is unset.
func TestWorkspaceNameReadsOrgName(t *testing.T) {
	t.Setenv(envWorkspaceName, "")
	if got := WorkspaceName(); got != "" {
		t.Errorf("unset: %q, want \"\"", got)
	}
	t.Setenv(envWorkspaceName, " Acme\tStudio\x07 ")
	if got := WorkspaceName(); got != "Acme Studio" {
		t.Errorf("ORG_NAME set: %q, want \"Acme Studio\"", got)
	}
}

// A long name is cut by characters, never mid-character, and never left
// ending in the space before a cut word.
func TestWorkspaceNameIsCappedByCharacters(t *testing.T) {
	got := cleanWorkspaceName(strings.Repeat("é", maxWorkspaceNameLen+20))
	if n := len([]rune(got)); n != maxWorkspaceNameLen {
		t.Fatalf("%d characters, want %d", n, maxWorkspaceNameLen)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("cut through a character: %q", got)
	}
	if got := cleanWorkspaceName(strings.Repeat("a", maxWorkspaceNameLen-1) + " tail"); strings.HasSuffix(got, " ") {
		t.Fatalf("ends in a space: %q", got)
	}
}
