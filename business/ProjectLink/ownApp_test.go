package business

import (
	"testing"

	unfurlBusiness "github.com/akashc777/OneCamp/business/Unfurl"
)

// onApp sets this workspace's web app address for a test.
func onApp(t *testing.T, base string) {
	t.Helper()
	old := frontendBase
	t.Cleanup(func() { frontendBase = old })
	frontendBase = func() string { return base }
}

// A link to one of this workspace's own pages (a table, a doc) isn't read:
// the page is behind sign-in, so a read would only see the sign-in page and
// keep its words. It's named by its title, else by its host, and no picture
// is fetched.
func TestAddDoesNotReadThisWorkspacesOwnPages(t *testing.T) {
	onApp(t, "https://work.example.com")
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "Sign in", Description: "The all-in-one workspace", SiteName: "OneCamp", IconURL: "https://work.example.com/icon.png"}}
	f.install(t)
	v, err := add(t, "https://work.example.com/app/tables/7b0c", "Deals")
	if err != nil {
		t.Fatal(err)
	}
	if f.unfurled != 0 || len(f.fetched) != 0 || f.kept != 0 {
		t.Errorf("this workspace's own page was read: unfurled %d, pictures %v, kept %d", f.unfurled, f.fetched, f.kept)
	}
	if v.Title != "Deals" || v.Description != "" || v.SiteName != "" || v.HasIcon {
		t.Errorf("the link: %+v", v)
	}
	if _, err := add(t, "https://work.example.com/app/tables/7b0c?view=2", ""); err != nil || f.made.Title != "work.example.com" || f.unfurled != 0 {
		t.Errorf("an own page with no title: %v %q, unfurled %d", err, f.made.Title, f.unfurled)
	}
}

// Only the configured address exactly is this workspace's: a host that only
// looks like it, ends like it, or differs in scheme or port is another site,
// read as any other is.
func TestAddReadsAPageThatOnlyLooksLikeThisWorkspaces(t *testing.T) {
	for base, raw := range map[string]string{
		"https://work.example.com":      "https://work.example.com.evil.test/app/tables/1",
		"https://example.com":           "https://work.example.com/app/tables/1",
		"https://work.example.com:8443": "https://work.example.com/app/tables/1",
		"http://work.example.com":       "https://work.example.com/app/tables/1",
		"":                              "https://work.example.com/app/tables/1",
	} {
		onApp(t, base)
		f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "Elsewhere"}}
		f.install(t)
		v, err := add(t, raw, "")
		if err != nil || f.unfurled != 1 || v.Title != "Elsewhere" {
			t.Errorf("app %q, link %s: unfurled %d, %+v, %v", base, raw, f.unfurled, v, err)
		}
	}
}
