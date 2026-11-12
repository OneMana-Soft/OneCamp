package provider

import "testing"

func TestPaletteColor(t *testing.T) {
	cases := map[string]string{
		"": "", "none": "", "chartreuse": "",
		// monday.com's status colours, ClickUp's hex, short hex.
		"#fdab3d": "amber", "#00C875": "emerald", "#e2445c": "rose", "#1bbc9c": "teal", "#f00": "red",
		// Greys, near-white and near-black are slate.
		"#c4c4c4": "slate", "#ffffff": "slate", "#111": "slate",
		"#12345": "", "#zzzzzz": "",
		// Names: Asana, Notion, Trello.
		"blue-green": "teal", "cool-gray": "slate", "hot-pink": "pink", "magenta": "pink", "yellow-orange": "amber",
		"brown": "amber", "default": "slate", "blue_background": "blue",
		"sky_dark": "sky", "green_light": "green", "Purple": "purple",
	}
	for in, want := range cases {
		if got := PaletteColor(in); got != want {
			t.Errorf("PaletteColor(%q) = %q, want %q", in, got, want)
		}
	}
}

// A currency a source names by code is one only when ISO 4217 has it: three
// letters that aren't a currency ("hrs", "xyz") name none.
func TestCurrencyCode(t *testing.T) {
	cases := map[string]string{"EUR": "EUR", "eur": "EUR", " gbp ": "GBP", "DKK": "DKK", "TOP": "TOP", "$": "USD", "€": "EUR", "xyz": "", "hrs": "", "": "", "euro": ""}
	for in, want := range cases {
		if got := CurrencyCode(in); got != want {
			t.Errorf("CurrencyCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCurrencyOf(t *testing.T) {
	cases := map[string]string{"$": "USD", "€": "EUR", "₹": "INR", " £ ": "GBP", "eur": "EUR", "USD": "USD", "dollars": "", "": "", "e1": "", "hrs": "", "pcs": "",
		"Rs.": "INR", "A$": "AUD", "chf": "CHF", "kr": "SEK", "top": "", "all": "", "cup": ""}
	for in, want := range cases {
		if got := CurrencyOf(in); got != want {
			t.Errorf("CurrencyOf(%q) = %q, want %q", in, got, want)
		}
	}
}
