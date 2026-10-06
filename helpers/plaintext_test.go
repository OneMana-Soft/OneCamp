package helpers

import "testing"

func TestNormaliseText(t *testing.T) {
	in := "\r\n  Hello  \r\n\r\n\r\n\r\n- one  \n- two\n\n"
	if got, want := NormaliseText(in), "Hello\n\n- one\n- two"; got != want {
		t.Errorf("NormaliseText = %q, want %q", got, want)
	}
}

func TestPlainTextToHTML(t *testing.T) {
	in := "Shipped <b>SSO</b>.\nMore soon.\n\nDone:\n- Import & sync\n* Pricing page\n\nThanks"
	want := "<p>Shipped &lt;b&gt;SSO&lt;/b&gt;.<br>More soon.</p><p>Done:</p><ul><li>Import &amp; sync</li><li>Pricing page</li></ul><p>Thanks</p>"
	if got := PlainTextToHTML(in); got != want {
		t.Errorf("PlainTextToHTML =\n%s\nwant\n%s", got, want)
	}
	if got := PlainTextToHTML(" \r\n "); got != "" {
		t.Errorf("empty text made %q", got)
	}
	// A signature's "-- " is not a list.
	if got := PlainTextToHTML("Thanks\n-- \nSam"); got != "<p>Thanks<br>--<br>Sam</p>" {
		t.Errorf("signature = %q", got)
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine("A  short\n\nnote", 40); got != "A short note" {
		t.Errorf("OneLine = %q", got)
	}
	if got := OneLine("abcdefghij", 5); got != "abcd…" {
		t.Errorf("cut = %q", got)
	}
	if got := OneLine("añoñoñoño", 4); got != "año…" {
		t.Errorf("cut by characters, not bytes = %q", got)
	}
	if got := OneLine("abc", 0); got != "" {
		t.Errorf("no room = %q", got)
	}
}
