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

// Rich text as text PlainTextToHTML reads back the same: a paragraph or a
// heading ends with a blank line, a line break is one, a list item is a line
// of its own (numbered in an ordered list, ticked or not in a to-do list),
// inline marks go but the spaces around them stay, and a code block keeps
// its lines.
func TestHTMLToText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"<p>Shipped <strong>SSO</strong> &amp; <em>SCIM</em>.<br>More soon.</p><p>Thanks</p>", "Shipped SSO & SCIM.\nMore soon.\n\nThanks"},
		{"<h2>Plan</h2><ul><li><p>Import</p></li><li><p>Sync</p><ul><li><p>both ways</p></li></ul></li></ul><p>Then</p>", "Plan\n\n- Import\n- Sync\n- both ways\n\nThen"},
		{"<ol><li><p>Draft</p></li><li><p>Review</p></li></ol>", "1. Draft\n2. Review"},
		{`<ul data-type="taskList"><li data-checked="true" data-type="taskItem"><label><input type="checkbox" checked></label><div><p>Book it</p></div></li><li data-checked="false"><div><p>Pay</p></div></li></ul>`, "- [x] Book it\n- [ ] Pay"},
		{"<pre><code>go test\n  ./...</code></pre><p> a  b </p>", "go test\n  ./...\n\na b"},
		{"plain words", "plain words"},
		{"", ""},
	} {
		if got := HTMLToText(c.in); got != c.want {
			t.Errorf("HTMLToText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// What PlainTextToHTML makes comes back from its text as it was.
	made := PlainTextToHTML("Shipped <b>SSO</b>.\nMore soon.\n\nDone:\n- Import & sync\n- Pricing page\n\nThanks")
	if got := PlainTextToHTML(HTMLToText(made)); got != made {
		t.Errorf("read back as\n%s\nwant\n%s", got, made)
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
