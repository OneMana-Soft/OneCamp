package businness

import (
	"errors"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// An icon is one emoji or one of OneCamp's glyphs, never words or markup.
func TestADocIconIsAnEmojiOrAGlyph(t *testing.T) {
	for icon, want := range map[string]bool{
		"📘":                true,
		"🚀":                true,
		"👩🏽‍💻":             true, // skin tone, joiner
		"❤️":               true, // presentation selector
		"🇮🇳":               true, // a flag's two halves
		"🏴󠁧󠁢󠁳󠁣󠁴󠁿":          true, // a subdivision's flag, in tags
		"1️⃣":              true, // a keycap
		"©️":               true,
		"glyph:rocket":     true,
		"glyph:book-open":  true,
		"glyph:rocket:sky": true,
		"glyph:rocket:red": false, // not a camp colour
		"glyph:Rocket":     false,
		"glyph:":           false,
		"a":                false,
		"1":                false, // a digit without its keycap
		"ab":               false,
		"中":                false, // a letter
		"<b>x</b>":         false,
		"📘 notes":          false,
		"📘📘📘📘📘📘📘📘📘📘📘📘📘": false, // too long
		"‍": false, // a joiner alone
	} {
		err := Look{Icon: ptr(icon)}.Check("d")
		if got := err == nil; got != want {
			t.Errorf("%q: %v, want accepted %v", icon, err, want)
		}
	}
	if err := (Look{Icon: ptr("")}).Check("d"); err != nil {
		t.Errorf("taking the icon away: %v", err)
	}
}

// A cover is the gallery's or an image uploaded to this doc; its position is
// a percentage; the font is one of three; and a change says what it changes.
func TestADocLookIsChecked(t *testing.T) {
	const doc = "6f1c2b3a-1111-4222-8333-444455556666"
	const file = "0a0b0c0d-1111-4222-8333-444455556666"
	cases := []struct {
		name string
		l    Look
		ok   bool
	}{
		{"a gallery cover", Look{Cover: ptr("gallery:dusk")}, true},
		{"an upload to this doc", Look{Cover: ptr(doc + "/" + file)}, true},
		{"the same, in capitals", Look{Cover: ptr("6F1C2B3A-1111-4222-8333-444455556666/" + file)}, true},
		{"an upload to another doc", Look{Cover: ptr("7f1c2b3a-1111-4222-8333-444455556666/" + file)}, false},
		{"a file that isn't one", Look{Cover: ptr(doc + "/x")}, false},
		{"an address", Look{Cover: ptr("https://example.com/a.png")}, false},
		{"no cover", Look{Cover: ptr("")}, true},
		{"the top of the cover", Look{CoverOffset: ptr(0)}, true},
		{"the bottom of the cover", Look{CoverOffset: ptr(100)}, true},
		{"past the bottom", Look{CoverOffset: ptr(101)}, false},
		{"above the top", Look{CoverOffset: ptr(-1)}, false},
		{"serif", Look{Font: ptr("serif")}, true},
		{"mono", Look{Font: ptr("mono")}, true},
		{"the default font", Look{Font: ptr("")}, true},
		{"comic sans", Look{Font: ptr("comic")}, false},
		{"full width", Look{FullWidth: ptr(true)}, true},
		{"small text", Look{SmallText: ptr(false)}, true},
		{"nothing", Look{}, false},
	}
	for _, tc := range cases {
		err := tc.l.Check(doc)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok %v", tc.name, err, tc.ok)
		}
		var bad ErrBadLook
		if err != nil && (!errors.As(err, &bad) || bad.Why == "") {
			t.Errorf("%s: %v doesn't say why", tc.name, err)
		}
	}
	if f, ok := (Look{Cover: ptr(doc + "/" + file)}).CoverFile(); !ok || f != file {
		t.Errorf("an upload's file: %q %v", f, ok)
	}
	for _, l := range []Look{{Cover: ptr("gallery:sky")}, {Cover: ptr("")}, {Icon: ptr("📘")}} {
		if f, ok := l.CoverFile(); ok {
			t.Errorf("%+v names a file: %q", l, f)
		}
	}
	if got := (Look{Icon: ptr("📘"), CoverOffset: ptr(20), SmallText: ptr(true)}).changed(); got != "icon,cover,small_text" {
		t.Errorf("changed: %q", got)
	}
}
