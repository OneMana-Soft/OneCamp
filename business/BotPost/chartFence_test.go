package botpost

import (
	"strings"
	"testing"
)

func TestChartDivHTML(t *testing.T) {
	// Valid object → compact, escaped div.
	div, ok := chartDivHTML(`{"type":"bar","labels":["a","b"],"series":[{"values":[1,2]}]}`)
	if !ok {
		t.Fatal("valid chart spec should produce a div")
	}
	if !strings.HasPrefix(div, `<div data-type="chart" data-spec="`) || !strings.HasSuffix(div, `"></div>`) {
		t.Fatalf("unexpected div shape: %q", div)
	}
	// The JSON is HTML-attribute escaped (quotes become entities).
	if strings.Contains(div, `data-spec="{"`) {
		t.Fatalf("spec attribute must be escaped, got %q", div)
	}

	// Invalid / non-object → no div.
	if _, ok := chartDivHTML("not json"); ok {
		t.Error("invalid JSON must not produce a div")
	}
	if _, ok := chartDivHTML("[1,2,3]"); ok {
		t.Error("a JSON array (not an object) must not produce a div")
	}
	if _, ok := chartDivHTML("   "); ok {
		t.Error("blank body must not produce a div")
	}
}

func TestSplitChartFences(t *testing.T) {
	// No fence → single text segment.
	segs := splitChartFences("just text\n\nmore text")
	if len(segs) != 1 || segs[0].isChart {
		t.Fatalf("expected one text segment, got %+v", segs)
	}

	// Text, chart, text.
	in := "Here is the breakdown:\n```chart\n{\"type\":\"bar\"}\n```\nDone."
	segs = splitChartFences(in)
	if len(segs) != 3 {
		t.Fatalf("expected 3 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].isChart || !segs[1].isChart || segs[2].isChart {
		t.Fatalf("segment classification wrong: %+v", segs)
	}
	if strings.TrimSpace(segs[1].jsonBody) != `{"type":"bar"}` {
		t.Fatalf("chart body = %q", segs[1].jsonBody)
	}

	// Unclosed fence → treated as text (streaming in progress).
	if hasChartFence("intro\n```chart\n{\"type\":\"bar\"") {
		t.Error("an unclosed chart fence must not count as a chart")
	}
}

func TestStreamHTML_RendersChartEmbed(t *testing.T) {
	in := "Deals by stage:\n```chart\n{\"type\":\"bar\",\"labels\":[\"Won\"],\"series\":[{\"values\":[2]}]}\n```"
	got := streamHTML(in, false)
	if !strings.Contains(got, `<div data-type="chart" data-spec="`) {
		t.Fatalf("expected a chart embed div, got %q", got)
	}
	if !strings.Contains(got, "<p>Deals by stage:</p>") {
		t.Fatalf("expected the intro paragraph, got %q", got)
	}
	// No raw fence should leak into the HTML.
	if strings.Contains(got, "```") {
		t.Fatalf("raw fence leaked into HTML: %q", got)
	}
}

func TestStreamHTML_InvalidChartFallsBackToText(t *testing.T) {
	in := "```chart\nnot valid json\n```"
	got := streamHTML(in, false)
	if strings.Contains(got, `data-type="chart"`) {
		t.Fatalf("invalid spec must not produce a chart div, got %q", got)
	}
	// It should still render something (the raw block as escaped text).
	if !strings.Contains(got, "not valid json") {
		t.Fatalf("invalid chart block should render as text, got %q", got)
	}
}

func TestStreamHTML_PlainTextUnchanged(t *testing.T) {
	// The fast path must exactly match the original plain-text behavior.
	got := streamHTML("para one\nline two\n\npara two", false)
	if !strings.Contains(got, "<p>para one<br/>line two</p>") || !strings.Contains(got, "<p>para two</p>") {
		t.Fatalf("plain-text rendering regressed: %q", got)
	}
	if strings.Contains(got, streamCursor) {
		t.Fatalf("final text must not carry a cursor: %q", got)
	}
}

// Model-agnostic fence handling: whatever style/placement the model emits, a
// valid closed chart spec must render as a chart embed (not raw text).
func TestSplitChartFences_ModelAgnostic(t *testing.T) {
	spec := `{"type":"bar","labels":["A","B"],"series":[{"values":[1,2]}]}`
	cases := []struct {
		name string
		in   string
	}{
		{"inline within prose", "Here is a chart: ```chart " + spec + "``` thanks!"},
		{"uppercase label", "```Chart\n" + spec + "\n```"},
		{"all-caps label", "```CHART\n" + spec + "\n```"},
		{"space after backticks", "``` chart\n" + spec + "\n```"},
		{"four backticks", "````chart\n" + spec + "\n````"},
		{"parenthesized spec", "```chart(" + spec + ")```"},
		{"prefix then fence same line", "data: ```chart " + spec + " ```"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !hasChartFence(c.in) {
				t.Fatalf("expected a chart fence to be detected in %q", c.in)
			}
			got := streamHTML(c.in, false)
			if !strings.Contains(got, `<div data-type="chart" data-spec="`) {
				t.Fatalf("expected a chart embed div, got %q", got)
			}
			// No raw code fence (backticks) should leak into the rendered HTML.
			if strings.Contains(got, "`") {
				t.Fatalf("raw fence leaked into HTML: %q", got)
			}
		})
	}
}

// "charter" (a word that merely starts with "chart") must NOT be treated as a
// chart fence.
func TestChartMarker_WordBoundary(t *testing.T) {
	if hasChartFence("```charter\n{\"type\":\"bar\"}\n```") {
		t.Error("`charter` must not be read as a chart fence")
	}
}

// TestBadgeHTML_UnbadgedPlainTextRendersChart guards the non-streamed reply
// path: an AI agent's plain-text answer that contains a closed ```chart block
// (posted via PostCommentToPostAsBot / PostToChannelAsBot, which funnel through
// badgeHTML) must render an inline chart embed node — matching the streamed
// path — rather than escaping the fence into literal text.
func TestBadgeHTML_UnbadgedPlainTextRendersChart(t *testing.T) {
	in := "Deals by stage:\n```chart\n{\"type\":\"bar\",\"labels\":[\"Won\"],\"series\":[{\"values\":[2]}]}\n```"
	got := badgeHTML(in, "")
	if !strings.Contains(got, `<div data-type="chart" data-spec="`) {
		t.Fatalf("expected a chart embed div from an unbadged plain-text reply, got %q", got)
	}
	// The lead-in prose is still escaped and wrapped in a paragraph.
	if !strings.Contains(got, "<p>Deals by stage:</p>") {
		t.Fatalf("expected the lead-in prose as a paragraph, got %q", got)
	}
}

// TestBadgeHTML_MultiParagraphPlainText verifies the unbadged plain-text path
// splits paragraphs (blank-line separated) and converts single newlines to
// <br/>, so a non-streamed multi-paragraph reply reads the same as a streamed
// one (previously it was flattened into a single <p> with literal newlines).
func TestBadgeHTML_MultiParagraphPlainText(t *testing.T) {
	got := badgeHTML("para one\nline two\n\npara two", "")
	if !strings.Contains(got, "<p>para one<br/>line two</p>") || !strings.Contains(got, "<p>para two</p>") {
		t.Fatalf("multi-paragraph plain text not rendered correctly: %q", got)
	}
}

// TestBadgeHTML_PlainTextEscaped confirms ordinary plain text (no chart) is
// HTML-escaped, so nothing a model emits can inject markup.
func TestBadgeHTML_PlainTextEscaped(t *testing.T) {
	got := badgeHTML("hello <b>world</b> & friends", "")
	if strings.Contains(got, "<b>") {
		t.Fatalf("plain text must be escaped, got %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") || !strings.Contains(got, "&amp;") {
		t.Fatalf("expected escaped entities, got %q", got)
	}
}

// RenderWithCharts is the one split every surface shares. The renderer it is
// handed sees only prose, gets told which run is last, and never sees a chart.
func TestRenderWithChartsHandsTheRendererOnlyProse(t *testing.T) {
	in := "Before\n\n```chart\n{\"type\":\"bar\",\"labels\":[\"a\"],\"series\":[{\"name\":\"n\",\"values\":[1]}]}\n```\nAfter"
	var runs []string
	var lasts []bool
	out := RenderWithCharts(in, func(run string, last bool) string {
		runs = append(runs, strings.TrimSpace(run))
		lasts = append(lasts, last)
		return "<p>" + strings.TrimSpace(run) + "</p>"
	})
	if len(runs) != 2 || runs[0] != "Before" || runs[1] != "After" {
		t.Fatalf("prose runs = %q, want [Before After]", runs)
	}
	if lasts[0] || !lasts[1] {
		t.Errorf("last flags = %v, want only the final run marked", lasts)
	}
	if !strings.Contains(out, `<div data-type="chart" data-spec="`) {
		t.Errorf("chart was not rendered as an embed: %s", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("a fence leaked into the output: %s", out)
	}
	if strings.Index(out, "Before") > strings.Index(out, "data-type") || strings.Index(out, "data-type") > strings.Index(out, "After") {
		t.Errorf("order was not preserved: %s", out)
	}
}

// With no chart the whole text is one run, and it is the last one.
func TestRenderWithChartsPassesPlainTextThroughOnce(t *testing.T) {
	calls := 0
	out := RenderWithCharts("just words", func(run string, last bool) string {
		calls++
		if !last {
			t.Error("the only run must be the last run")
		}
		return run
	})
	if calls != 1 || out != "just words" {
		t.Fatalf("calls=%d out=%q", calls, out)
	}
}

// A block that looks like a chart but does not parse reaches the renderer as
// the model wrote it, so the reader sees the words rather than nothing.
func TestRenderWithChartsHandsAnInvalidChartToTheRenderer(t *testing.T) {
	in := "```chart\n{\"type\":}\n```"
	// The opening brace never balances, so this is not a chart at all and the
	// whole text is one run; a balanced but non-JSON body takes the other path.
	in2 := "```chart\n{not json}\n```"
	for _, tc := range []string{in, in2} {
		out := RenderWithCharts(tc, func(run string, _ bool) string { return "[" + run + "]" })
		if !strings.Contains(out, "```chart") {
			t.Errorf("%q: the invalid block did not reach the renderer verbatim: %s", tc, out)
		}
		if strings.Contains(out, "data-type") {
			t.Errorf("%q: an invalid spec was drawn: %s", tc, out)
		}
	}
}

// ChartsAsText names a chart for a plain-text surface instead of dumping the
// first line of its JSON.
func TestChartsAsTextNamesTheChart(t *testing.T) {
	in := "Tickets rose.\n```chart\n{\"type\":\"bar\",\"title\":\"Tickets by week\",\"labels\":[\"a\"],\"series\":[{\"name\":\"n\",\"values\":[1]}]}\n```\nDone."
	got := ChartsAsText(in)
	if got != "Tickets rose.\n[chart: Tickets by week]\nDone." {
		t.Fatalf("got %q", got)
	}
	untitled := "```chart {\"type\":\"pie\",\"labels\":[\"a\"],\"series\":[{\"name\":\"n\",\"values\":[1]}]}```"
	if got := ChartsAsText(untitled); got != "[chart]" {
		t.Errorf("untitled chart = %q, want [chart]", got)
	}
	if got := ChartsAsText("no chart here"); got != "no chart here" {
		t.Errorf("plain text changed: %q", got)
	}
	if got := ChartsAsText("```chart\n{not json}\n```"); !strings.Contains(got, "not json") {
		t.Errorf("an invalid block was replaced instead of kept: %q", got)
	}
}
