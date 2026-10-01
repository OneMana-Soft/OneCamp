package authService

import (
	"os"
	"strings"
	"testing"
)

func TestMatchAdminGroup(t *testing.T) {
	cases := []struct {
		name    string
		allow   []string
		claimed []string
		want    bool
	}{
		{"empty allow → never admin", nil, []string{"admins"}, false},
		{"empty claimed → never admin", []string{"admins"}, nil, false},
		{"exact match", []string{"admins"}, []string{"admins"}, true},
		{"case-insensitive match", []string{"Admins"}, []string{"ADMINS"}, true},
		{"trims whitespace", []string{" admins "}, []string{"admins"}, true},
		{"multiple claimed, one matches", []string{"admins"}, []string{"users", "admins", "viewers"}, true},
		{"no match", []string{"admins"}, []string{"users"}, false},
		{"DN-style matches short name (ops choose)", []string{"CN=Admins,OU=Groups"}, []string{"cn=admins,ou=groups"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchAdminGroup(tc.allow, tc.claimed)
			if got != tc.want {
				t.Fatalf("MatchAdminGroup(%v, %v) = %v, want %v", tc.allow, tc.claimed, got, tc.want)
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	cases := map[string][]string{
		"":            nil,
		"a":           {"a"},
		"a,b":         {"a", "b"},
		" a , b , c ": {"a", "b", "c"},
		",a,,b,":      {"a", "b"},
	}
	for in, want := range cases {
		got := splitCSV(in)
		if !equalSlices(got, want) {
			t.Errorf("splitCSV(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFrontendBaseURL_ResolutionOrder(t *testing.T) {
	saved := snapshotEnv("FRONTEND_DOMAIN", "FE_HOST_DOMAIN", "COOKIE_SECURE")
	defer restoreEnv(saved)

	// Unset → fallback localhost:3001 with http.
	for _, k := range []string{"FRONTEND_DOMAIN", "FE_HOST_DOMAIN", "COOKIE_SECURE"} {
		_ = os.Unsetenv(k)
	}
	if got := FrontendBaseURL(); got != "http://localhost:3001" {
		t.Errorf("default fallback = %q, want http://localhost:3001", got)
	}

	// FRONTEND_DOMAIN beats FE_HOST_DOMAIN.
	t.Setenv("FRONTEND_DOMAIN", "app.example.com")
	t.Setenv("FE_HOST_DOMAIN", "old.example.com")
	if got := FrontendBaseURL(); got != "https://app.example.com" {
		t.Errorf("FRONTEND_DOMAIN preference = %q, want https://app.example.com", got)
	}

	// FE_HOST_DOMAIN can be a full URL (legacy support).
	_ = os.Unsetenv("FRONTEND_DOMAIN")
	t.Setenv("FE_HOST_DOMAIN", "https://legacy.example.com/")
	if got := FrontendBaseURL(); got != "https://legacy.example.com" {
		t.Errorf("legacy full-URL = %q", got)
	}

	// localhost / COOKIE_SECURE=false → http scheme.
	_ = os.Unsetenv("FE_HOST_DOMAIN")
	t.Setenv("FRONTEND_DOMAIN", "localhost:3001")
	if got := FrontendBaseURL(); got != "http://localhost:3001" {
		t.Errorf("localhost host = %q, want http://localhost:3001", got)
	}

	t.Setenv("FRONTEND_DOMAIN", "app.example.com")
	t.Setenv("COOKIE_SECURE", "false")
	if got := FrontendBaseURL(); got != "http://app.example.com" {
		t.Errorf("COOKIE_SECURE=false override = %q", got)
	}
}

func TestIsRedirectAllowed(t *testing.T) {
	saved := snapshotEnv("FRONTEND_DOMAIN", "FE_HOST_DOMAIN", "COOKIE_SECURE")
	defer restoreEnv(saved)
	t.Setenv("FRONTEND_DOMAIN", "app.example.com")
	_ = os.Unsetenv("FE_HOST_DOMAIN")
	_ = os.Unsetenv("COOKIE_SECURE")

	cases := map[string]bool{
		"":                                 false,
		"https://app.example.com":          true,
		"https://app.example.com/app":      true,
		"https://app.example.com/app?x=1":  true,
		"https://attacker.com":             false,
		"https://app.example.com.evil.com": false, // prefix attack
		"http://app.example.com":           false, // wrong scheme
		"//app.example.com/app":            false,
	}
	for target, want := range cases {
		if got := IsRedirectAllowed(target); got != want {
			t.Errorf("IsRedirectAllowed(%q) = %v, want %v", target, got, want)
		}
	}
}

// --- helpers ---

func snapshotEnv(keys ...string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = os.Getenv(k)
	}
	return out
}

func restoreEnv(saved map[string]string) {
	for k, v := range saved {
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// silence linter: imported below for assertions if anyone adds them.
var _ = strings.TrimSpace
