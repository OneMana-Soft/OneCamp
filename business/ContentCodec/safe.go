package contentcodec

import (
	"regexp"
	"strconv"
	"strings"
)

// safeHref reports whether a link may be kept: http, https, mailto or tel, or
// no scheme at all (a path, a fragment, a relative link). The web app's
// isSafeHref (lib/utils/safeHref.ts), the same rules: the scheme is looked for
// with every control and space-like character taken out, and again after each
// round of decoding character references.
func safeHref(href string) bool {
	if trimSpace(href) == "" {
		return false
	}
	candidate := href
	for round := 0; ; round++ {
		scheme, ok := schemeOf(candidate)
		if ok && !allowedSchemes[scheme] {
			return false
		}
		next := decodeRefsOnce(candidate)
		if next == candidate {
			return true
		}
		if round == maxDecodes {
			return false
		}
		candidate = next
	}
}

const maxDecodes = 8

var allowedSchemes = map[string]bool{"http": true, "https": true, "mailto": true, "tel": true}

func isIgnoredInScheme(r rune) bool {
	switch {
	case r <= 0x20, r >= 0x7f && r <= 0xa0, r == 0xad, r == 0x1680, r == 0x180e,
		r >= 0x2000 && r <= 0x200f, r >= 0x2028 && r <= 0x202f, r >= 0x205f && r <= 0x206f,
		r == 0x3000, r == 0xfeff:
		return true
	}
	return false
}

func schemeOf(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		if !isIgnoredInScheme(r) {
			b.WriteRune(r)
		}
	}
	t := b.String()
	if t == "" {
		return "", false
	}
	c := t[0]
	if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return "", false
	}
	for i := 1; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '+', c == '.', c == '-':
			continue
		case c == ':':
			return strings.ToLower(t[:i]), true
		default:
			return "", false
		}
	}
	return "", false
}

var (
	hexRef   = regexp.MustCompile(`(?i)&#x([0-9a-f]+);?`)
	decRef   = regexp.MustCompile(`&#(\d+);?`)
	namedRef = regexp.MustCompile(`(?i)&([a-z][a-z0-9]*);`)
)

var namedInScheme = map[string]string{
	"amp": "&", "colon": ":", "tab": "\t", "newline": "\n", "nbsp": "\u00a0", "plus": "+",
	"period": ".", "hyphen": "-", "dash": "-", "sol": "/", "num": "#", "quest": "?",
}

func codePoint(s string, base int) string {
	v, err := strconv.ParseUint(s, base, 32)
	if err != nil || v > 0x10ffff {
		return ""
	}
	if v >= 0xd800 && v <= 0xdfff {
		return ""
	}
	return string(rune(v))
}

func decodeRefsOnce(s string) string {
	s = hexRef.ReplaceAllStringFunc(s, func(m string) string {
		return codePoint(hexRef.FindStringSubmatch(m)[1], 16)
	})
	s = decRef.ReplaceAllStringFunc(s, func(m string) string {
		return codePoint(decRef.FindStringSubmatch(m)[1], 10)
	})
	return namedRef.ReplaceAllStringFunc(s, func(m string) string {
		return namedInScheme[strings.ToLower(namedRef.FindStringSubmatch(m)[1])]
	})
}

// keptClasses are the classes content may carry (lib/contentClasses.ts).
var keptClasses = map[string]bool{
	"text-node": true, "heading-node": true, "block-node": true, "list-node": true, "inline": true,
	"link": true, "mention": true, "channel-mention": true, "entity-mention": true,
	"hover:cursor-pointer": true, "task-list": true, "task-item": true,
}

var languageClass = regexp.MustCompile(`(?i)^language-[a-z0-9+#._-]{1,32}$`)

// contentClasses is the classes in value content may keep.
func contentClasses(value string) string {
	var kept []string
	for _, c := range strings.Fields(value) {
		if keptClasses[c] || languageClass.MatchString(c) {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, " ")
}

// safeImageSrc reports whether an image source may stay in a doc: a web
// address, a path, or (until the asset pass uploads it) a data: image. A
// blob: address only ever meant something in the tab that made it.
func safeImageSrc(src string, allowData bool) bool {
	s := trimSpace(src)
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "data:") {
		return allowData && strings.HasPrefix(low, "data:image/") && !strings.HasPrefix(low, "data:image/svg")
	}
	if strings.HasPrefix(low, "blob:") {
		return false
	}
	scheme, ok := schemeOf(s)
	if !ok {
		return true
	}
	return scheme == "http" || scheme == "https"
}
