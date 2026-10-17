package contentcodec

import (
	"runtime"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/tests/mdpathology"
	"github.com/akashc777/OneCamp/tests/timing"
)

// The doc reader's own work, past the Markdown guard, grows as its input
// does. Emphasis nested a thousand deep, text read in tens of thousands of
// runs, and inline HTML tags left open read in time linear in their length:
// four times the bytes take at most ten times the time on a loaded machine (a
// quadratic read takes sixteen), the two sizes timed back to back
// (timing.Ratio), with the reading of prose as long as the floor. They were
// quadratic: 16 KB of nested emphasis took 1.4 s, each text carrying a mark
// for every level above it; 130 KB of "[a" took 8.6 s, joined one run at a
// time.
func TestTheDocReaderIsLinearInWhatItReads(t *testing.T) {
	const small, large = 8 << 10, 32 << 10
	prose := mdpathology.Prose(small)
	runtime.GC()
	floor := timing.Fastest(3, func() { readMarkdown(prose, nil) })
	for _, c := range []struct {
		name string
		make func(size int) string
	}{
		{"nested emphasis", func(n int) string {
			return strings.Repeat("*a **a ", n/14) + "b" + strings.Repeat(" a** a*", n/14)
		}},
		{"text in many runs", func(n int) string { return strings.Repeat("[a", n/2) }},
		{"inline HTML left open", func(n int) string { return strings.Repeat("<b>a <i>b ", n/10) }},
		{"emphasis in many runs", func(n int) string { return strings.Repeat("*a* ", n/4) }},
	} {
		s, l := c.make(small), c.make(large)
		if grew := timing.Ratio(3, floor, func() { readMarkdown(s, nil) }, func() { readMarkdown(l, nil) }); grew > 10 {
			t.Errorf("%s: reading grew %.1f times from 8 KB to 32 KB (prose: %v)", c.name, grew, floor)
		}
	}
}

// The marks a text carries are one of each type, the innermost, as Normalize
// keeps them, whether they come from Markdown or from inline HTML, and a
// closing tag closes the latest of its own kind. Read as the reader read
// them before marks were kept one of each (caf3c684, these and eight more
// compared byte for byte).
func TestATextCarriesTheInnermostMarkOfEachType(t *testing.T) {
	const link = `<a target="_blank" rel="noopener noreferrer nofollow" href=`
	for md, want := range map[string]string{
		"*a **b *c* d** e*":                   "<p><em>a </em><strong><em>b c d</em></strong><em> e</em></p>",
		"<b>a <b>b</b> c</b> d":               "<p><strong>a b c</strong> d</p>",
		"<b>open <i>both</b> close</i> after": "<p><strong>open <em>both</em></strong><em> close</em> after</p>",
		`<a href="https://h.test">html [md](https://m.test) link</a>`: "<p>" + link + `"https://h.test">html </a>` +
			link + `"https://m.test">md</a>` + link + `"https://h.test"> link</a></p>`,
		"<u>under <i>it <b>bold</b></i> plain</u> end": "<p><u>under </u><em><u>it </u></em><strong><em><u>bold</u></em></strong><u> plain</u> end</p>",
	} {
		if got := ToHTML(readMarkdown(md, nil)); got != want {
			t.Errorf("%q read as %s, want %s", md, got, want)
		}
	}
}
