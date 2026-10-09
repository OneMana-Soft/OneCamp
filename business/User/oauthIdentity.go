package business

// Who Google or GitHub vouches for, and only when they do.
//
// Signing in through either names the person by email address, and an
// invited or allowed address joins the workspace with it (AdmitOAuthUser). So
// the address has to be one the provider has verified belongs to that
// account: an unverified one is anyone's to type into their own Google or
// GitHub account. Neither was checked. GitHub's sign-in took the account's
// public address, else the primary one from its list, else the last in the
// list; and with no public address set (GitHub's default), reading it
// dereferenced nothing and the sign-in failed with a panic.
//
// A GitHub account has several addresses, and the one a workspace knows is
// often not its primary: someone invited at work whose primary address is
// personal was turned away as not invited. Any address GitHub has verified
// now counts, the one the workspace knows first (githubAddressToAdmit).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// ErrUnverifiedEmail refuses a sign-in whose provider hasn't verified the
// address it names. The words are for the person signing in.
var ErrUnverifiedEmail = errors.New("your account's email address isn't verified with the provider you signed in with; verify it there, then sign in again")

// ErrNotInvited refuses a sign-in from an address that is neither a member's,
// on the sign-up allow-list, nor invited (AdmitOAuthUser). Its own error, so
// the sign-in page can say so rather than guess. The words are for the person.
var ErrNotInvited = errors.New("this email address isn't invited to this workspace; ask an administrator to invite you, or to add it to the sign-up allow-list")

// ErrAddressNotASCII refuses an address with characters outside ASCII:
// whoever vouches for it, it is not matched to an account or admitted
// (helpers.NormalizeEmail says why). The words are for the person.
var ErrAddressNotASCII = errors.New("this email address has characters other than plain letters, digits and symbols, so OneCamp can't match it to an account; sign in with an account whose address is written in plain ASCII")

// GitHubEmail is one entry of GitHub's GET /user/emails.
type GitHubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// How well the workspace knows an address, best first: a member's, one with
// a live invitation, one on the allow-list. unknownAddress is none of these.
const (
	knownMember = iota
	knownInvited
	knownAllowed
	unknownAddress = -1
)

// githubAddressToAdmit is the address a GitHub sign-in goes ahead with: of
// the addresses GitHub has verified, the one the workspace knows best (known
// ranks it), the primary first among equals; else the primary, else the first
// verified one, which the workspace then turns away as not invited. An
// unverified address is never used: it is anyone's to add to their account.
// One with characters outside ASCII is never ranked (known is not asked about
// it), so it is used only when nothing else is verified, and then refused.
// ok is false when GitHub has verified none. Pure.
func githubAddressToAdmit(emails []GitHubEmail, known func(addr string) int) (string, bool) {
	best, bestRank := "", unknownAddress
	fallback := ""
	for _, primaryPass := range []bool{true, false} {
		for _, e := range emails {
			addr := strings.TrimSpace(e.Email)
			if !e.Verified || addr == "" || e.Primary != primaryPass {
				continue
			}
			if fallback == "" {
				fallback = addr
			}
			if !helpers.AddressIsASCII(addr) {
				continue
			}
			if rank := known(addr); rank != unknownAddress && (bestRank == unknownAddress || rank < bestRank) {
				best, bestRank = addr, rank
			}
		}
	}
	if best != "" {
		return best, true
	}
	return fallback, fallback != ""
}

// GitHubSignInAddress is the address a GitHub sign-in goes ahead with, of
// the account's addresses (githubAddressToAdmit), as the workspace knows them.
func GitHubSignInAddress(ctx context.Context, emails []GitHubEmail) (string, bool) {
	return githubAddressToAdmit(emails, workspaceKnows(ctx))
}

// workspaceKnows ranks an address for githubAddressToAdmit from the stores.
// A lookup that fails counts as not knowing it.
func workspaceKnows(ctx context.Context) func(addr string) int {
	return func(addr string) int {
		addr = helpers.NormalizeEmail(addr)
		// A LIVE member's: a deactivated account's address ranked first, so
		// someone whose old account was deactivated was signed in as it, and
		// refused, though GitHub had verified another address invited here.
		if exists, deactivated, err := domain.MemberAccountState(ctx, addr); err == nil && exists && !deactivated {
			return knownMember
		}
		if usable, err := domain.HasUsableInvitation(ctx, addr); err == nil && usable {
			return knownInvited
		}
		if allowListed(addr) {
			return knownAllowed
		}
		return unknownAddress
	}
}

// allowListed reports whether an address is on the workspace's allow-list
// (Admin > General) as itself, which admits it through Google or GitHub
// without an invitation. A domain entry ("@acme.com") is not asked here: it
// admits through Google alone (admittedByDomain).
func allowListed(addr string) bool {
	return onAllowList(addr, settingsBusiness.AllowedUsers())
}

// onAllowList is allowListed against given entries: an entry that is this
// address. An address outside ASCII is on no list. Pure.
func onAllowList(addr string, entries []string) bool {
	addr = helpers.NormalizeEmail(addr)
	if !helpers.AddressIsASCII(addr) {
		return false
	}
	for _, entry := range entries {
		if entry = helpers.NormalizeEmail(entry); entry != "" && entry == addr {
			return true
		}
	}
	return false
}

// allowListedDomain is admittedByDomain against the workspace's allow-list.
func allowListedDomain(addr, provider, hostedDomain string) bool {
	return admittedByDomain(addr, provider, hostedDomain, settingsBusiness.AllowedUsers())
}

// admittedByDomain reports whether a domain entry on the allow-list
// ("@acme.com") admits this sign-in, without an invitation: only through
// Google, only when Google says the account is managed by that domain's
// Google Workspace (hostedDomain, the ID token's hd), and only for an address
// at exactly that domain.
//
// A verified address alone isn't enough. Anyone can make a Google account
// with any address they can read mail at, and GitHub keeps an address
// verified long after its owner leaves the company; only the Workspace's own
// accounts are the company's to give and take away. Pure.
func admittedByDomain(addr, provider, hostedDomain string, entries []string) bool {
	if provider != AuthRecipeMethodGoogle {
		return false
	}
	addr, domain := helpers.NormalizeEmail(addr), helpers.NormalizeEmail(hostedDomain)
	at := strings.LastIndexByte(addr, '@')
	if domain == "" || at <= 0 || !helpers.AddressIsASCII(addr) || addr[at+1:] != domain {
		return false
	}
	for _, entry := range entries {
		if helpers.NormalizeEmail(entry) == "@"+domain {
			return true
		}
	}
	return false
}

// displayName is the name a person joining is given: the first of names that
// isn't blank, else their address up to the @. Pure.
func displayName(email string, names ...*string) string {
	for _, n := range names {
		if n != nil && strings.TrimSpace(*n) != "" {
			return strings.TrimSpace(*n)
		}
	}
	if at := strings.IndexByte(email, '@'); at > 0 {
		return email[:at]
	}
	return email
}

// githubGet reads one GitHub API resource with the signed-in person's token.
func githubGet(ctx context.Context, client *http.Client, token, url string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("github %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
