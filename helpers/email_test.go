package helpers

import "testing"

func TestAnAddressIsKeptOneWay(t *testing.T) {
	for in, want := range map[string]string{
		"  Ana.Silva@Example.COM ": "ana.silva@example.com",
		"ana@example.com":          "ana@example.com",
		"":                         "",
	} {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// Unicode lowercasing turns the Kelvin sign into "k" and the dotted capital I
// into "i" with a combining dot, so an address spelt with them became someone
// else's. Only A to Z are lowercased, and such an address is not ASCII, which
// is what keeps it from being matched or admitted.
func TestOnlyASCIILettersAreLowercased(t *testing.T) {
	for _, in := range []string{"Kate@example.com", "İnci@example.com", "ana@Kexample.com"} {
		if got := NormalizeEmail(in); got != in {
			t.Errorf("NormalizeEmail(%q) = %q: a character outside A-Z was changed", in, got)
		}
		if AddressIsASCII(in) {
			t.Errorf("%q reads as ASCII", in)
		}
	}
	if got := NormalizeEmail("KATE@EXAMPLE.COM"); got != "kate@example.com" || !AddressIsASCII(got) {
		t.Errorf("an ASCII address: %q", got)
	}
}
