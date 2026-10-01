package botpost

// diffFence.go — turns a fenced ```diff block in an agent's reply into an atomic
// diff node the chat/channel renderer draws as a reviewable, side-by-side diff.
//
// WHY. The code tools already produce a proposed patch (code_analyze returns a
// diff and never commits), and until now that patch arrived as a wall of
// monospace text. Review therefore happened somewhere else — on GitHub, after
// the PR existed — which is the moment the reader leaves the conversation the
// change was discussed in. Rendering the diff in place is what lets a reviewer
// stay where the context is.
//
// It deliberately mirrors chartFence.go: same marker tolerance (shared through
// findFenceMarker), same atomic-div contract, same self-healing behaviour. An
// unclosed or unconvincing block is left as ordinary text, so a half-streamed
// patch renders as plain text until it closes rather than flashing a broken
// node.
//
// Safety: identical to the chart node. The div goes into the stream's own
// trusted HTML and data-diff carries only attribute-escaped text that the
// frontend reads as data and renders as rows, never as markup.

import (
	"html"
	"strings"
)

// maxDiffAttrBytes bounds what goes into a single attribute. Patches can be
// enormous, and a multi-megabyte attribute would be paid for on every render of
// the surrounding thread. Matches the 64KB ceiling read_repo_file already uses,
// so the two limits agree about what "one readable file's worth" means. A larger
// patch falls back to text, which still renders, just without the review UI.
const maxDiffAttrBytes = 64 * 1024

// diffSegment is a run of the reply that is either ordinary text or a diff
// block. raw is the original text (fences included for a diff segment, so an
// unusable block can be rendered verbatim); body is the extracted patch.
type diffSegment struct {
	raw    string
	body   string
	isDiff bool
}

// splitDiffFences splits text into ordered text/diff segments on closed
// ```diff … ``` blocks. With no complete block the whole text comes back as a
// single text segment.
func splitDiffFences(text string) []diffSegment {
	var segs []diffSegment
	i := 0
	for {
		start, after := findFenceMarker(text, i, "diff")
		if start < 0 {
			break
		}
		// The patch starts on the line AFTER the marker line, so anything else
		// on the marker line (a filename, say) is not treated as patch content.
		nl := strings.IndexByte(text[after:], '\n')
		if nl < 0 {
			break // marker with no newline after it: not a closed block
		}
		bodyStart := after + nl + 1
		bodyEnd, closeEnd := findClosingFence(text, bodyStart)
		if bodyEnd < 0 {
			break // still streaming, or never closed
		}
		if start > i {
			segs = append(segs, diffSegment{raw: text[i:start]})
		}
		segs = append(segs, diffSegment{
			raw:    text[start:closeEnd],
			body:   text[bodyStart:bodyEnd],
			isDiff: true,
		})
		i = closeEnd
	}
	if len(segs) == 0 {
		return []diffSegment{{raw: text}}
	}
	if i < len(text) {
		segs = append(segs, diffSegment{raw: text[i:]})
	}
	return segs
}

// findClosingFence returns the byte offset where the body ends and the offset
// just past the closing fence line, or (-1, -1) when the block never closes. The
// closing fence is a line of three or more backticks and nothing else.
func findClosingFence(s string, from int) (bodyEnd, after int) {
	for i := from; i <= len(s); {
		nl := strings.IndexByte(s[i:], '\n')
		line, next := s[i:], len(s)
		if nl >= 0 {
			line, next = s[i:i+nl], i+nl+1
		}
		if t := strings.TrimSpace(line); len(t) >= 3 && strings.Trim(t, "`") == "" {
			return i, next
		}
		if nl < 0 {
			break
		}
		i = next
	}
	return -1, -1
}

// looksLikeDiff reports whether a body is plausibly a unified diff. Without this
// any ```diff block, including an empty one a model emitted by mistake, would
// render as an empty review pane — worse than showing the text.
func looksLikeDiff(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"),
			strings.HasPrefix(line, "diff --git"),
			strings.HasPrefix(line, "+++ "),
			strings.HasPrefix(line, "--- "):
			return true
		case len(line) > 1 && (line[0] == '+' || line[0] == '-'):
			// A bare +/- line counts, which is what a model emits when asked for
			// a fragment rather than a full patch.
			return true
		}
	}
	return false
}

// diffDivHTML returns the atomic diff node for a patch body, or ("", false) when
// the body is not usable and the caller should render the block as text.
func diffDivHTML(body string) (string, bool) {
	patch := strings.Trim(body, "\n")
	patch = strings.TrimRight(patch, " \t\n")
	if patch == "" || len(patch) > maxDiffAttrBytes || !looksLikeDiff(patch) {
		return "", false
	}
	return `<div data-type="diff" data-diff="` + html.EscapeString(patch) + `"></div>`, true
}

// renderDiffAware renders one run of text, converting any closed, valid ```diff
// block into a diff node. Text either side is rendered by the ordinary
// paragraph renderer, and the typing cursor stays on the last TEXT run so it
// never lands inside a diff.
func renderDiffAware(text string, typing bool) string {
	// Cheap guard first, matching hasChartFence. This runs for EVERY agent reply,
	// and the overwhelming majority contain no fence at all; without this each one
	// pays a full byte scan looking for a backtick that is not there.
	if !strings.Contains(text, "`") {
		return renderParagraphs(text, typing)
	}
	segs := splitDiffFences(text)
	if len(segs) == 1 && !segs[0].isDiff {
		return renderParagraphs(text, typing)
	}
	lastText := -1
	for i, s := range segs {
		if !s.isDiff {
			lastText = i
		}
	}
	var b strings.Builder
	for i, s := range segs {
		if s.isDiff {
			if div, ok := diffDivHTML(s.body); ok {
				b.WriteString(div)
				continue
			}
			b.WriteString(renderParagraphs(s.raw, false))
			continue
		}
		b.WriteString(renderParagraphs(s.raw, typing && i == lastText))
	}
	return b.String()
}
