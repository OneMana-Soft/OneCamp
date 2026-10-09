package business

import (
	"context"
	"testing"
)

// GitHub's word for an address is its list, and only an entry it has
// verified: an unverified one is anyone's to add to their account. Of the
// verified ones, the address the workspace knows is the one that signs in,
// whichever is primary.
func TestGithubSignsInWithAVerifiedAddressTheWorkspaceKnows(t *testing.T) {
	knows := func(ranks map[string]int) func(string) int {
		return func(addr string) int {
			if r, ok := ranks[addr]; ok {
				return r
			}
			return unknownAddress
		}
	}
	nobody := knows(nil)
	cases := []struct {
		name   string
		emails []GitHubEmail
		known  func(string) int
		want   string
		ok     bool
	}{
		{"primary and verified", []GitHubEmail{{"me@own.example", true, true}}, nobody, "me@own.example", true},
		{"found among others", []GitHubEmail{{"old@x.example", false, true}, {"me@own.example", true, true}}, nobody, "me@own.example", true},
		{"someone else's address, added but unverified", []GitHubEmail{{"me@own.example", true, true}, {"ceo@victim.example", false, false}},
			knows(map[string]int{"ceo@victim.example": knownMember}), "me@own.example", true},
		{"invited at work, personal primary", []GitHubEmail{{"me@home.example", true, true}, {"me@work.example", false, true}},
			knows(map[string]int{"me@work.example": knownInvited}), "me@work.example", true},
		{"a member's address before an allowed one", []GitHubEmail{{"me@allowed.example", true, true}, {"me@member.example", false, true}},
			knows(map[string]int{"me@allowed.example": knownAllowed, "me@member.example": knownMember}), "me@member.example", true},
		{"primary unverified, another verified", []GitHubEmail{{"ceo@victim.example", true, false}, {"me@own.example", false, true}}, nobody, "me@own.example", true},
		{"none verified", []GitHubEmail{{"ceo@victim.example", true, false}}, knows(map[string]int{"ceo@victim.example": knownMember}), "", false},
		{"nothing", nil, nobody, "", false},
	}
	for _, c := range cases {
		got, ok := githubAddressToAdmit(c.emails, c.known)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestSomeoneJoiningIsAlwaysNamed(t *testing.T) {
	blank, name, login := "  ", "Ada Lovelace", "ada"
	cases := []struct {
		names []*string
		want  string
	}{
		{[]*string{&name, &login}, "Ada Lovelace"},
		{[]*string{nil, &login}, "ada"}, // GitHub, no display name set
		{[]*string{&blank, nil}, "ada.l"},
		{nil, "ada.l"},
	}
	for _, c := range cases {
		if got := displayName("ada.l@example.test", c.names...); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

// An unknown provider used to leave no user and no error, and the callback
// then dereferenced the missing user.
func TestAnUnknownProviderIsRefusedNotAPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	if _, _, _, err := OAuthCallback(context.Background(), "code", "myspace"); err == nil {
		t.Fatal("an unknown provider must be refused")
	}
}

// An address entry on the allow-list admits that address, through Google or
// GitHub. A domain entry ("@acme.com") admits only through Google, only an
// account Google says acme.com's Google Workspace manages (the ID token's
// hd), and only an address at exactly acme.com. A verified address alone is
// not enough: anyone can make a Google account with an address they can read,
// and GitHub keeps an address verified after its owner leaves the company.
func TestAnAllowListAdmitsADomainOnlyThroughItsGoogleWorkspace(t *testing.T) {
	entries := []string{"@Acme.com", "sam@example.org", "@"}
	for addr, want := range map[string]bool{
		"sam@example.org": true,
		"ann@example.org": false,
		"ana@acme.com":    false, // a domain entry doesn't list the address itself
		"nobody":          false,
	} {
		if got := onAllowList(addr, entries); got != want {
			t.Errorf("listed %s: %v, want %v", addr, got, want)
		}
	}
	for _, c := range []struct {
		addr, provider, hd string
		want               bool
	}{
		{"ana@acme.com", "google", "acme.com", true},
		{"Ana@ACME.com", "google", "Acme.com", true},
		{"ana@acme.com", "google", "", false},                   // a Google account no organisation manages
		{"ana@acme.com", "google", "other.example", false},      // managed by another Workspace
		{"ana@mail.acme.com", "google", "acme.com", false},      // an address at another domain
		{"ana@mail.acme.com", "google", "mail.acme.com", false}, // not on the list
		{"ana@acme.com", "github", "acme.com", false},           // GitHub never admits by a domain
		{"\u212Ana@acme.com", "google", "acme.com", false},      // not ASCII
	} {
		if got := admittedByDomain(c.addr, c.provider, c.hd, entries); got != c.want {
			t.Errorf("%s through %s (hd %q): %v, want %v", c.addr, c.provider, c.hd, got, c.want)
		}
	}
}
