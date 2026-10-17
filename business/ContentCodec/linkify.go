package contentcodec

import (
	"strings"
)

// linkify turns bare web addresses in text into links, as GitHub does
// (its "extended autolinks": http://…, https://…, www.…), in text that is
// not already a link or code. The TypeScript codec runs the same rules
// (lib/codec/linkify.ts) rather than markdown-it's or goldmark's own, so the
// two agree on every address.
func linkify(n *Node) {
	if n == nil || n.Type == "codeBlock" {
		return
	}
	if len(n.Content) == 0 {
		return
	}
	var out []*Node
	changed := false
	for _, c := range n.Content {
		if c.Type != "text" || hasMark(c.Marks, "link") || hasMark(c.Marks, "code") {
			linkify(c)
			out = append(out, c)
			continue
		}
		parts := autolinks(c.Text)
		if len(parts) == 1 && parts[0].href == "" {
			out = append(out, c)
			continue
		}
		changed = true
		for _, p := range parts {
			t := &Node{Type: "text", Text: p.text, Marks: append([]*Mark{}, c.Marks...)}
			if p.href != "" {
				t.Marks = append(t.Marks, &Mark{Type: "link", Attrs: map[string]any{"href": p.href}})
			}
			out = append(out, t)
		}
	}
	if changed {
		n.Content = out
	}
}

type linkPart struct {
	text string
	href string
}

func isAutolinkSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// autolinks splits text into plain runs and addresses.
func autolinks(s string) []linkPart {
	var parts []linkPart
	last := 0
	for i := 0; i < len(s); i++ {
		if i > 0 {
			prev := s[i-1]
			if !(isAutolinkSpace(prev) || prev == '*' || prev == '_' || prev == '~' || prev == '(') {
				continue
			}
		}
		rest := s[i:]
		low := strings.ToLower(rest[:min(len(rest), 8)])
		var prefix string
		switch {
		case strings.HasPrefix(low, "https://"):
			prefix = rest[:8]
		case strings.HasPrefix(low, "http://"):
			prefix = rest[:7]
		case strings.HasPrefix(low, "www."):
			prefix = ""
		default:
			continue
		}
		end := i
		for end < len(s) && !isAutolinkSpace(s[end]) && s[end] != '<' && s[end] >= 0x20 && s[end] != 0x7f {
			end++
		}
		cand := trimAutolink(s[i:end])
		domainStart := len(prefix)
		if !validAutolinkDomain(cand[domainStart:]) {
			continue
		}
		if prefix != "" && len(cand) == len(prefix) {
			continue
		}
		if i > last {
			parts = append(parts, linkPart{text: s[last:i]})
		}
		href := cand
		if prefix == "" {
			href = "http://" + cand
		}
		parts = append(parts, linkPart{text: cand, href: href})
		last = i + len(cand)
		i = last - 1
	}
	if last < len(s) || len(parts) == 0 {
		parts = append(parts, linkPart{text: s[last:]})
	}
	return parts
}

// trimAutolink drops what GitHub leaves out of the end of an address:
// trailing punctuation, a ) with no ( to match, an entity reference.
func trimAutolink(s string) string {
	for {
		if s == "" {
			return s
		}
		c := s[len(s)-1]
		switch c {
		case '?', '!', '.', ',', ':', '*', '_', '~', '\'', '"':
			s = s[:len(s)-1]
			continue
		case ')':
			if strings.Count(s, ")") > strings.Count(s, "(") {
				s = s[:len(s)-1]
				continue
			}
		case ';':
			if amp := strings.LastIndexByte(s, '&'); amp >= 0 && isAlnumRun(s[amp+1:len(s)-1]) {
				s = s[:amp]
				continue
			}
		}
		return s
	}
}

func isAlnumRun(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// validAutolinkDomain: segments of letters, digits, - and _, at least two of
// them, with no _ in the last two.
func validAutolinkDomain(s string) bool {
	end := len(s)
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '?' || s[i] == '#' || s[i] == ':' {
			end = i
			break
		}
	}
	domain := s[:end]
	if domain == "" {
		return false
	}
	segs := strings.Split(domain, ".")
	if len(segs) < 2 {
		return false
	}
	for i, seg := range segs {
		if seg == "" {
			return false
		}
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c >= 0x80
			if !ok {
				return false
			}
			if c == '_' && i >= len(segs)-2 {
				return false
			}
		}
	}
	return true
}
