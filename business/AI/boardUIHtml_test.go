package business

import (
	"strings"
	"testing"
)

// A generated screen keeps its design and loses whatever could act, however
// it's written: each of these got through the pattern-based sanitizer this
// replaced, or would have.
func TestAGeneratedScreenLosesWhatCouldAct(t *testing.T) {
	cases := []struct{ name, in, mustNot string }{
		{"a handler after a slash", `<img/onerror=alert(1) src=x>`, "onerror"},
		{"a handler after a closing quote", `<img src="x"onerror="alert(1)">`, "onerror"},
		{"a handler in SVG", `<svg><animate onbegin="alert(1)" attributeName="x"/></svg>`, "onbegin"},
		{"a script", `<div>hi<script>alert(1)</script></div>`, "alert"},
		{"a script in SVG", `<svg><script>alert(1)</script></svg>`, "alert"},
		{"a frame with a document", `<iframe srcdoc="<script>alert(1)</script>"></iframe>`, "iframe"},
		{"a scheme behind a reference", `<a href="jav&#x09;ascript:alert(1)">x</a>`, "javascript"},
		{"a scheme behind spaces", `<a href=" javascript:alert(1)">x</a>`, "javascript"},
		{"an SVG link", `<svg><a xlink:href="javascript:alert(1)"><text>x</text></a></svg>`, "javascript"},
		{"a form's action", `<form action="javascript:alert(1)"><button formaction="javascript:alert(2)">go</button></form>`, "javascript"},
		{"a page as a link", `<a href="data:text/html,<script>alert(1)</script>">x</a>`, "data:"},
		{"an embedded object", `<object data="x.swf"></object><embed src="x.swf">`, "swf"},
		{"a base", `<base href="https://evil.example/">`, "evil"},
		{"a refresh", `<meta http-equiv="refresh" content="0;url=https://evil.example">`, "evil"},
	}
	for _, c := range cases {
		got := sanitizeGeneratedHTML(c.in)
		if strings.Contains(strings.ToLower(got), c.mustNot) {
			t.Errorf("%s: %q kept %q", c.name, got, c.mustNot)
		}
	}
}

func TestAGeneratedScreenKeepsItsDesign(t *testing.T) {
	cases := []struct{ name, in string }{
		{"layout and styling", `<div class="flex items-center gap-4 p-6" style="max-width:420px">Hello &amp; welcome</div>`},
		{"an icon", `<svg viewbox="0 0 24 24" class="h-5 w-5"><path d="M4 12h16" stroke="currentColor"></path></svg>`},
		{"an inline image", `<img src="data:image/png;base64,iVBORw0KGgo=" alt="logo"/>`},
		{"links", `<a href="https://example.com/pricing">Pricing</a><a href="#top">Top</a><a href="mailto:hi@example.com">Mail</a>`},
		{"a form", `<form><label for="e">Email</label><input id="e" type="email" placeholder="you@company.com"/><button type="submit">Join</button></form>`},
		{"CSS with child selectors", `<style>.card > .title { font-weight: 600 }</style>`},
	}
	for _, c := range cases {
		if got := sanitizeGeneratedHTML(c.in); got != c.in {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.in)
		}
	}
}
