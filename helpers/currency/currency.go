// Package currency is the one place the server says what a currency is: its
// ISO 4217 code, the sign a sheet shows for it, and the currency a sign
// names. A table's number format, its exports, an import's guess and the
// importers all ask here, so they agree; it imports nothing of OneCamp's, so
// any of them may import it.
package currency

import (
	"fmt"
	"strings"

	xcurrency "golang.org/x/text/currency"
)

// added are the codes ISO 4217 took on after the library's list was made:
// the ouguiya, the leone and the bolívar as they were renamed, the Caribbean
// guilder and Zimbabwe's gold. Money people keep accounts in today, so they
// are codes here too.
var added = map[string]bool{"MRU": true, "SLE": true, "VED": true, "VES": true, "XCG": true, "ZWG": true}

// Code is s as an ISO 4217 code: three letters the standard has, in
// capitals, whatever case and spaces s came with; "" when s isn't one. A
// code the standard has retired (DEM, HRK) is still one, for the accounts
// kept in it; the code ISO 4217 keeps for no currency at all isn't.
// Pure.
func Code(s string) string {
	code := strings.ToUpper(strings.TrimSpace(s))
	if len(code) != 3 || code == "XXX" {
		return ""
	}
	if added[code] {
		return code
	}
	if _, err := xcurrency.ParseISO(code); err != nil {
		return ""
	}
	return code
}

// Sign is what a sheet shows before an amount in code, a code as Code gives
// it: the currency's sign where it has one, told apart from its namesakes
// ("A$", "CA$", "JP¥"), else the code itself ("CHF"). The plain dollar is
// the US dollar, as a table shows it. "" when code isn't a currency. Pure.
func Sign(code string) string {
	switch {
	case code == "" || Code(code) != code:
		return ""
	case code == "USD":
		return "$"
	case added[code]:
		return code
	}
	unit, _ := xcurrency.ParseISO(code)
	return fmt.Sprint(xcurrency.Symbol(unit))
}

// signs are the signs sources write for money, each with the currency it
// most often names, in small letters; init adds the sign Sign writes for
// each currency, so what a sheet was written with reads back.
var signs = map[string]string{
	"$": "USD", "us$": "USD", "€": "EUR", "£": "GBP", "₹": "INR", "rs": "INR", "rs.": "INR", "¥": "JPY", "₩": "KRW",
	"₽": "RUB", "₺": "TRY", "₪": "ILS", "₫": "VND", "₱": "PHP", "฿": "THB", "zł": "PLN",
	"kr": "SEK", "r$": "BRL", "a$": "AUD", "c$": "CAD", "₦": "NGN",
}

func init() {
	seen := map[string]bool{}
	for it := xcurrency.Query(xcurrency.Historical, xcurrency.NonTender); it.Next(); {
		code := it.Unit().String()
		if seen[code] {
			continue
		}
		seen[code] = true
		sign := strings.ToLower(Sign(code))
		if _, taken := signs[sign]; sign != "" && sign != strings.ToLower(code) && !taken {
			signs[sign] = code
		}
	}
}

// OfSign is the code of the currency a sign names ("€", "US$", "Rs."), or
// "" when it names none: a code isn't a sign (Code reads those), and nor is
// a unit ("km"). Pure.
func OfSign(s string) string {
	return signs[strings.ToLower(strings.TrimSpace(s))]
}
