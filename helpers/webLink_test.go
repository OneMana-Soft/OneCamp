package helpers

import (
	"net/url"
	"testing"
)

// A web link is an http or https address to a host, whatever the scheme's
// case; nothing else a person might paste is one.
func TestWebLinkIsAnHTTPAddressToAHost(t *testing.T) {
	for s, want := range map[string]bool{
		"https://example.com/thanks?from=form": true,
		"http://example.com":                   true,
		"HTTPS://Example.com":                  true,
		"https://user:pass@example.com":        true, // a web link; business/Form's redirect refuses the user on top
		"":                                     false,
		"example.com":                          false,
		"/thanks":                              false,
		"https://":                             false,
		"https:example.com":                    false,
		"mailto:team@example.com":              false,
		"javascript:alert(1)":                  false,
		"ftp://example.com/file":               false,
		"https://exa mple.com":                 false,
	} {
		u, ok := WebLink(s)
		if ok != want {
			t.Errorf("WebLink(%q) = %v, want %v", s, ok, want)
		}
		if ok && u.Host == "" {
			t.Errorf("WebLink(%q) gave no host", s)
		}
	}
}

// An address already read is a web one by the same rule: a relative one, or
// one with no host, isn't, until it's resolved against a page's own.
func TestIsWebURLAsksTheSameOfAnAddressAlreadyRead(t *testing.T) {
	page, _ := url.Parse("https://example.com/blog/post")
	img, _ := url.Parse("/img/cover.png")
	if IsWebURL(img) || IsWebURL(nil) {
		t.Fatal("a relative address, or none, is not a web one")
	}
	if got := page.ResolveReference(img); !IsWebURL(got) {
		t.Fatalf("%s resolved against its page should be a web address", got)
	}
	for _, s := range []string{"mailto:team@example.com", "https:example.com", "file:///etc/passwd"} {
		if u, _ := url.Parse(s); IsWebURL(u) {
			t.Errorf("IsWebURL(%q) = true", s)
		}
	}
}
