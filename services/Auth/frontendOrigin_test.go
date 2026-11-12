package authService

import "testing"

// FrontendOrigin is the web's origin from its host env, "" when none is set;
// FrontendBaseURL is it with the dev fallback, under the one scheme rule.
func TestFrontendOriginIsEmptyWithoutAWebHost(t *testing.T) {
	for _, c := range []struct {
		name, frontend, feHost, cookieSecure string
		origin, base                         string
	}{
		{"unset", "", "", "", "", "http://localhost:3001"},
		{"blank", "  ", " ", "", "", "http://localhost:3001"},
		{"a host", "team.example.test", "", "", "https://team.example.test", "https://team.example.test"},
		{"the older name", "", "team.example.test", "", "https://team.example.test", "https://team.example.test"},
		{"the first name wins", "a.example.test", "b.example.test", "", "https://a.example.test", "https://a.example.test"},
		{"a trailing slash", "team.example.test/", "", "", "https://team.example.test", "https://team.example.test"},
		{"a full URL", "http://team.example.test:8080/", "", "", "http://team.example.test:8080", "http://team.example.test:8080"},
		{"localhost", "localhost:3000", "", "", "http://localhost:3000", "http://localhost:3000"},
		{"insecure cookies", "team.example.test", "", "false", "http://team.example.test", "http://team.example.test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FRONTEND_DOMAIN", c.frontend)
			t.Setenv("FE_HOST_DOMAIN", c.feHost)
			t.Setenv("COOKIE_SECURE", c.cookieSecure)
			if got := FrontendOrigin(); got != c.origin {
				t.Errorf("FrontendOrigin() = %q, want %q", got, c.origin)
			}
			if got := FrontendBaseURL(); got != c.base {
				t.Errorf("FrontendBaseURL() = %q, want %q", got, c.base)
			}
		})
	}
}

// LocalHost is the scheme rule's own test for this machine.
func TestLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST:3001": true, "127.0.0.1:3000": true,
		"team.example.test": false, "10.0.0.5": false, "": false,
	} {
		if got := LocalHost(host); got != want {
			t.Errorf("LocalHost(%q) = %v, want %v", host, got, want)
		}
	}
}
