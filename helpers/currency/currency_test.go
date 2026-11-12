package currency

import (
	"strings"
	"testing"
)

// A currency is named by its ISO 4217 code, in any case and with spaces
// around it; anything else names none.
func TestACodeIsAnISO4217Code(t *testing.T) {
	for in, want := range map[string]string{
		"EUR": "EUR", " eur ": "EUR", "Gbp": "GBP", "INR": "INR", "USD": "USD", "AUD": "AUD", "CHF": "CHF", "JPY": "JPY",
		"": "", "EU": "", "eur1": "", "EURO": "", "XYZ": "", "€": "", "$": "", "other": "", "E R": "", "ÉUR": "",
	} {
		if got := Code(in); got != want {
			t.Errorf("Code(%q) = %q, want %q", in, got, want)
		}
	}
}

// The list is ISO 4217's, on purpose: a code it has retired still names the
// money old accounts were kept in, the codes it added after the library's
// list was made are money now, and XXX is its word for no currency at all.
func TestACodeRetiredOrNewIsStillOne(t *testing.T) {
	for _, code := range []string{"DEM", "FRF", "HRK", "MRU", "SLE", "VED", "VES", "XCG", "ZWG"} {
		if got := Code(code); got != code {
			t.Errorf("Code(%q) = %q, want it kept", code, got)
		}
	}
	if got := Code("XXX"); got != "" {
		t.Errorf("Code(XXX) = %q: it says there is no currency", got)
	}
}

// A sheet shows an amount with its currency's sign: the dollar and the
// rupee as a table always showed them, a sign that tells dollars apart, and
// the code where a currency has no sign.
func TestASignIsWhatASheetShows(t *testing.T) {
	for code, want := range map[string]string{
		"USD": "$", "INR": "₹", "EUR": "€", "GBP": "£", "AUD": "A$", "CAD": "CA$", "JPY": "JP¥", "CHF": "CHF", "SEK": "SEK", "VES": "VES",
		"": "", "XYZ": "", "eur": "",
	} {
		if got := Sign(code); got != want {
			t.Errorf("Sign(%q) = %q, want %q", code, got, want)
		}
	}
}

// A sign names its currency, however a source writes it; a code, a unit or
// a sign no one currency owns names none.
func TestASignNamesItsCurrency(t *testing.T) {
	for in, want := range map[string]string{
		"$": "USD", "US$": "USD", "us$": "USD", "€": "EUR", " £ ": "GBP", "₹": "INR", "Rs": "INR", "Rs.": "INR", "A$": "AUD", "CA$": "CAD",
		"¥": "JPY", "JP¥": "JPY", "CN¥": "CNY", "kr": "SEK", "zł": "PLN", "R$": "BRL",
		"": "", "EUR": "", "CHF": "", "¢": "", "km": "", "hrs": "",
	} {
		if got := OfSign(in); got != want {
			t.Errorf("OfSign(%q) = %q, want %q", in, got, want)
		}
	}
}

// What a sheet is written with reads back as the same currency, and sits
// inside a number format's quotes.
func TestASignReadsBackAsItsCode(t *testing.T) {
	for _, code := range []string{"USD", "INR", "EUR", "GBP", "AUD", "CAD", "NZD", "HKD", "SGD", "JPY", "CNY", "CHF", "SEK", "BRL", "MXN", "KRW", "ILS", "VND", "TWD", "XOF", "XAF", "XPF", "XCD", "ZAR", "AED", "VES"} {
		sign := Sign(code)
		if sign == "" || strings.ContainsAny(sign, `"[]-;\`) {
			t.Errorf("Sign(%q) = %q can't sit in a number format", code, sign)
		}
		if got := OfSign(sign); got != code && Code(sign) != code {
			t.Errorf("%s is written %q, which reads back as %q", code, sign, got)
		}
	}
}
