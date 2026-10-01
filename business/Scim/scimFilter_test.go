package business

import (
	"errors"
	"testing"
)

// The SCIM filter parser.
//
// WHY THE REFUSAL CASES ARE THE POINT. A directory asks `userName eq "someone@corp.com"` to find out
// whether it already has an account for that person. If an unsupported filter were ignored and the full
// list returned instead, the caller reads Resources[0] — an unrelated user — and concludes that is the
// person it asked about. It then updates, deactivates, or declines to create an account, all against the
// wrong identity. A parser that quietly degrades corrupts data; one that fails makes an operator fix a
// configuration. So every unsupported form below must produce an error, and a test asserting that is
// doing more work than the ones asserting the happy path.

func TestParseUserNameFilterAcceptsTheSupportedForm(t *testing.T) {
	cases := map[string]string{
		`userName eq "someone@corp.com"`: "someone@corp.com",
		// SCIM attribute names and operators are case-insensitive (RFC 7644 §3.4.2.2), and providers do
		// vary here.
		`username eq "someone@corp.com"`:                 "someone@corp.com",
		`USERNAME EQ "someone@corp.com"`:                 "someone@corp.com",
		`userName Eq "someone@corp.com"`:                 "someone@corp.com",
		`  userName   eq   "a@b.co"  `:                   "a@b.co",
		`userName eq "first.last+tag@sub.example.co.uk"`: "first.last+tag@sub.example.co.uk",
	}
	for filter, want := range cases {
		got, err := ParseUserNameFilter(filter)
		if err != nil {
			t.Errorf("%q was refused: %v", filter, err)
			continue
		}
		if got != want {
			t.Errorf("%q → %q, want %q", filter, got, want)
		}
	}
}

func TestParseUserNameFilterTreatsAbsenceAsListEverything(t *testing.T) {
	// No filter is not an error; it is a request for the whole directory, which is how an IdP does its
	// initial import.
	for _, filter := range []string{"", "   "} {
		got, err := ParseUserNameFilter(filter)
		if err != nil {
			t.Fatalf("%q should mean list-everything, got %v", filter, err)
		}
		if got != "" {
			t.Fatalf("%q produced a target %q", filter, got)
		}
	}
}

func TestParseUserNameFilterRefusesEverythingItCannotHonour(t *testing.T) {
	cases := []struct {
		name   string
		filter string
	}{
		{
			// A compound filter. Matching only the first clause would apply HALF the caller's condition
			// and return someone it explicitly excluded.
			name:   "compound and",
			filter: `userName eq "a@b.co" and active eq true`,
		},
		{
			name:   "compound or",
			filter: `userName eq "a@b.co" or userName eq "c@d.co"`,
		},
		{
			// A different operator. `co` is "contains", so honouring it as equality would match a
			// different set than was asked for.
			name:   "contains operator",
			filter: `userName co "corp.com"`,
		},
		{
			name:   "starts-with operator",
			filter: `userName sw "admin"`,
		},
		{
			name:   "not-equal operator",
			filter: `userName ne "a@b.co"`,
		},
		{
			// A different attribute entirely. Treating an externalId lookup as a userName lookup would
			// silently answer a question nobody asked.
			name:   "different attribute",
			filter: `externalId eq "okta-123"`,
		},
		{
			name:   "emails attribute",
			filter: `emails.value eq "a@b.co"`,
		},
		{
			// Unquoted value: not valid SCIM, and accepting it would mean guessing where the value ends.
			name:   "unquoted value",
			filter: `userName eq a@b.co`,
		},
		{
			name:   "empty quoted value",
			filter: `userName eq ""`,
		},
		{
			name:   "whitespace-only value",
			filter: `userName eq "   "`,
		},
		{
			name:   "missing operator",
			filter: `userName "a@b.co"`,
		},
		{
			name:   "grouped expression",
			filter: `(userName eq "a@b.co")`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUserNameFilter(tc.filter)
			if !errors.Is(err, ErrScimFilterUnsupported) {
				t.Fatalf("expected a refusal, got (%q, %v). Returning a target here would make the caller "+
					"act on the wrong user; returning no error with no target would make it act on ALL of them",
					got, err)
			}
		})
	}
}

func TestParseUserNameFilterUnescapesJSONStringEscapes(t *testing.T) {
	// SCIM filter values are JSON strings, so a quote inside one arrives escaped. Rare in an email
	// address, but the alternative is truncating the value at the escaped quote and looking up something
	// that is not what was asked for.
	got, err := ParseUserNameFilter(`userName eq "od\"d@corp.com"`)
	if err != nil {
		t.Fatalf("a legally escaped value was refused: %v", err)
	}
	if got != `od"d@corp.com` {
		t.Fatalf("got %q, want %q", got, `od"d@corp.com`)
	}
}

func TestParseUserNameFilterDoesNotTruncateAtAnEscapedQuote(t *testing.T) {
	// The specific bug a naive `"([^"]*)"` pattern produces: it would stop at the escaped quote and
	// return `od\` — a lookup for a user who does not exist, so an IdP would create a DUPLICATE account
	// for somebody already provisioned.
	got, err := ParseUserNameFilter(`userName eq "od\"d@corp.com"`)
	if err != nil {
		t.Fatal(err)
	}
	if got == `od\` || got == "od" {
		t.Fatalf("value truncated at the escaped quote: %q", got)
	}
}
