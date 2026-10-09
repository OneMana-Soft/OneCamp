package linear

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// The importing admin's Linear token goes with a file uploaded into Linear and
// with nothing else. A link attachment's URL is whatever someone in the Linear
// workspace typed; with a substring check, each of the last three got it.
func TestLinearSendsItsTokenOnlyToLinearUploads(t *testing.T) {
	for _, c := range []struct {
		url  string
		want string
	}{
		{"https://uploads.linear.app/1f2e/screenshot.png", "Bearer lin_api_x"},
		{"https://linear.app.evil.example/x", ""},
		{"https://uploads.linear.app.evil.example/x", ""},
		{"https://evil.example/?u=uploads.linear.app", ""},
		{"http://uploads.linear.app/1f2e/screenshot.png", ""},
	} {
		got := linearDownload(importProvider.SourceAttachment{URL: c.url}, "lin_api_x")
		if got.Headers["Authorization"] != c.want {
			t.Errorf("%s: Authorization %q, want %q", c.url, got.Headers["Authorization"], c.want)
		}
		if got.URL != c.url {
			t.Errorf("%s: fetched as %s", c.url, got.URL)
		}
	}
}
