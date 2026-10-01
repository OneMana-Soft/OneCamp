package botpost

// chartFence.go — turns a fenced ```chart block in an agent's plain-text reply
// into an atomic chart embed node the chat/channel renderer draws inline.
//
// Agents visualize data by emitting a ```chart block whose body is a compact
// JSON spec (see the AI chart prompt). On the assistant surface that block is
// rendered by MarkdownMessage; here — for messages posted into channels, threads
// and DMs, which render through the Tiptap editor — we translate a CLOSED,
// valid chart block into <div data-type="chart" data-spec="…"></div>. The Tiptap
// `chart` node parses that div and renders the same SVG chart. This mirrors how
// the meeting-recap agent emits a recording-embed node.
//
// Safety: the div is emitted into the stream's OWN trusted HTML (which is stored
// verbatim, not re-sanitized), and data-spec carries only HTML-attribute-escaped
// JSON that the frontend parses as data and renders as SVG numbers — it is never
// interpreted as markup, so there is no injection surface. An unclosed or
// invalid block is left as ordinary text (self-heals once the block completes),
// so a partial stream never flashes a broken chart.

import (
	"encoding/json"
	"html"
	"strings"
)

// chartSegment is a run of the reply that is either ordinary text or a chart
// block. raw is the original text (including fences for a chart segment, so an
// invalid block can be rendered verbatim); jsonBody is the extracted spec.
type chartSegment struct {
	isChart  bool
	raw      string
	jsonBody string
}

// splitChartFences scans text for closed ```chart … ``` blocks and returns the
// text split into ordered text/chart segments. When no complete chart block is
// present the whole text is returned as a single text segment (so streaming a
// half-written block renders as plain text until it closes).
//
// It is deliberately tolerant of how weaker models place the fence: the opening
// ```chart need NOT be alone on its own line, the JSON spec may sit on the same
// line as the fence, and the closing ``` may immediately follow the JSON — all
// of these render a chart:
//
//	```chart\n{…}\n```            (canonical)
//	here's a chart: ```chart {…}``` thanks!   (inline within prose)
//	```chart({…})```              (parenthesized)
//
// Detection is anchored on a BALANCED JSON object followed by a closing fence,
// so ordinary prose that merely mentions "```chart" without a valid spec is
// never misread as a chart, and a half-streamed block stays text until it
// closes.
func splitChartFences(text string) []chartSegment {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var segs []chartSegment
	i := 0

	for {
		markerAt, afterMarker := findChartMarker(text, i)
		if markerAt < 0 {
			break
		}

		// The spec is the first balanced {…} object after the marker.
		braceRel := strings.IndexByte(text[afterMarker:], '{')
		if braceRel < 0 {
			break // no JSON yet — streaming/incomplete; leave the rest as text
		}
		jsonStart := afterMarker + braceRel
		jsonBody, jsonEnd := extractBalancedObject(text, jsonStart)
		if jsonBody == "" {
			break // unbalanced — streaming/incomplete
		}

		// A closing ``` must follow, separated only by whitespace (tolerating a
		// stray ')' from the parenthesized form). Anything else means this
		// wasn't a real chart block — leave it as text.
		closeRel := strings.Index(text[jsonEnd:], "```")
		if closeRel < 0 {
			break // not closed yet
		}
		if gap := strings.Trim(strings.TrimSpace(text[jsonEnd:jsonEnd+closeRel]), ")"); gap != "" {
			break
		}
		closeAt := jsonEnd + closeRel + len("```")
		// Swallow any extra trailing backticks (e.g. a ````chart …```` block
		// closes with four) so none leak into the rendered text.
		for closeAt < len(text) && text[closeAt] == '`' {
			closeAt++
		}

		if pre := text[i:markerAt]; pre != "" {
			segs = append(segs, chartSegment{raw: pre})
		}
		segs = append(segs, chartSegment{isChart: true, raw: text[markerAt:closeAt], jsonBody: jsonBody})
		i = closeAt
	}

	if tail := text[i:]; tail != "" {
		segs = append(segs, chartSegment{raw: tail})
	}
	if len(segs) == 0 {
		segs = append(segs, chartSegment{raw: text})
	}
	return segs
}

// extractBalancedObject returns the first balanced {…} JSON object starting at
// s[start] (which must be '{'), and the index just past its closing brace. It
// is string- and escape-aware so a brace inside a string value doesn't close
// the object early. Returns ("", start) when start isn't '{' or never balances.
func extractBalancedObject(s string, start int) (string, int) {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return "", start
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], i + 1
			}
		}
	}
	return "", start
}

// chartDivHTML validates a chart spec JSON body and, on success, returns the
// atomic chart embed div carrying the compacted, attribute-escaped spec. Returns
// ("", false) when the body is not a usable JSON object, so the caller falls
// back to rendering the block as text.
func chartDivHTML(jsonBody string) (string, bool) {
	trimmed := strings.TrimSpace(jsonBody)
	if trimmed == "" {
		return "", false
	}
	// Compact + validate: must be a JSON object.
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return "", false
	}
	compact, err := json.Marshal(obj)
	if err != nil {
		return "", false
	}
	esc := html.EscapeString(string(compact))
	return `<div data-type="chart" data-spec="` + esc + `"></div>`, true
}

// RenderWithCharts renders model text whose closed, valid ```chart blocks
// become chart embed nodes, and whose remaining runs of text go through
// renderText. It is the one place the split happens, so every surface that
// turns model text into HTML draws a chart the same way and escapes the same
// text; the surfaces differ only in what they do with a run of prose.
//
// last is true for the final run of text, for a renderer that puts a typing
// cursor there (a cursor never belongs on a chart). A block that looks like a
// chart but does not parse is handed to renderText as it was written, so a
// reader sees what the model said rather than nothing.
//
// The split is done on the raw text, before any escaping, because the spec is
// JSON and a renderer that escaped quotes first would never find it again.
func RenderWithCharts(text string, renderText func(run string, last bool) string) string {
	if !hasChartFence(text) {
		return renderText(text, true)
	}
	segs := splitChartFences(text)
	lastText := -1
	for i, s := range segs {
		if !s.isChart {
			lastText = i
		}
	}
	var b strings.Builder
	for i, s := range segs {
		if s.isChart {
			if div, ok := chartDivHTML(s.jsonBody); ok {
				b.WriteString(div)
				continue
			}
			b.WriteString(renderText(s.raw, false))
			continue
		}
		b.WriteString(renderText(s.raw, i == lastText))
	}
	return b.String()
}

// ChartsAsText replaces each closed, valid chart block with a short placeholder
// naming the chart, for a surface that shows a reply as plain text: a feed
// summary, a notification line, a clipped preview. A reader of "[chart: Tickets
// by week]" knows there is a chart and what it shows; a reader of the first 140
// characters of its JSON knows neither. Text between charts is left as it was.
func ChartsAsText(text string) string {
	if !hasChartFence(text) {
		return text
	}
	var b strings.Builder
	for _, s := range splitChartFences(text) {
		if !s.isChart {
			b.WriteString(s.raw)
			continue
		}
		if _, ok := chartDivHTML(s.jsonBody); !ok {
			b.WriteString(s.raw)
			continue
		}
		b.WriteString(chartPlaceholder(s.jsonBody))
	}
	return b.String()
}

// chartPlaceholder is the one line that stands in for a chart in plain text.
func chartPlaceholder(jsonBody string) string {
	var spec struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(jsonBody), &spec); err == nil {
		if t := strings.TrimSpace(spec.Title); t != "" {
			return "[chart: " + t + "]"
		}
	}
	return "[chart]"
}

// RenderAgentBodyHTML renders an agent's plain-text reply as safe chat HTML,
// converting any closed, valid ```chart block into an inline chart embed node
// (and escaping everything else). Exported so other agent reply paths (the DM /
// group-chat coworker) render charts identically to channel posts, from one
// implementation. Empty input yields "<p></p>" to match the chat body contract.
func RenderAgentBodyHTML(text string) string {
	out := streamHTML(strings.TrimSpace(text), false)
	if out == "" {
		return "<p></p>"
	}
	return out
}

// hasChartFence reports whether text contains at least one complete chart block,
// so a caller can cheaply skip the split when there is nothing to convert.
func hasChartFence(text string) bool {
	if !strings.Contains(text, "`") {
		return false
	}
	for _, s := range splitChartFences(text) {
		if s.isChart {
			return true
		}
	}
	return false
}

// findChartMarker returns the index of the next chart fence marker at or after
// `from`, and the index just past it, or (-1, -1) when none. Model-agnostic:
// the marker is one or more backticks + optional spaces + the word "chart"
// (case-insensitive), so ```chart, ``` chart, ```Chart, ````chart, and
// ```CHART all match — whatever the model emits.
func findChartMarker(s string, from int) (start, after int) {
	return findFenceMarker(s, from, "chart")
}

// findFenceMarker is the same scan for ANY fence keyword, so a second block type
// (```diff) reuses the tolerance already built for models that write ``` diff,
// ```Diff or ````diff rather than duplicating it and drifting.
func findFenceMarker(s string, from int, kw string) (start, after int) {
	if from < 0 {
		from = 0
	}
	for i := from; i < len(s); i++ {
		if s[i] != '`' {
			continue
		}
		if l := fenceMarkerLen(s, i, kw); l > 0 {
			return i, i + l
		}
	}
	return -1, -1
}

// fenceMarkerLen returns the byte length of a fence marker for kw starting at
// s[i] (backticks + optional spaces + kw + a word boundary), or 0 when there is
// no marker there. The word boundary is what stops "charter" matching "chart"
// and "different" matching "diff". Pure.
func fenceMarkerLen(s string, i int, kw string) int {
	j := i
	ticks := 0
	for j < len(s) && s[j] == '`' {
		ticks++
		j++
	}
	if ticks < 3 {
		return 0
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j+len(kw) > len(s) || !strings.EqualFold(s[j:j+len(kw)], kw) {
		return 0
	}
	j += len(kw)
	if j < len(s) && isWordByte(s[j]) {
		return 0 // e.g. "charter" / "different" — not a fence for kw
	}
	return j - i
}

// isWordByte reports whether b is an ASCII letter or digit.
func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
