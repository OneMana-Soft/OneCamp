package business

// chartPrompt.go — teaches a model that it may VISUALIZE numeric results by
// emitting a fenced ```chart block. This is a pure prompt contribution: the
// model writes the block into its normal answer, and each surface that shows
// the answer decides how to draw it. The assistant's markdown renderer draws
// it directly (MarkdownMessage -> AgentChart); a channel post, DM, comment or
// document gets it as a chart embed node through botpost.RenderWithCharts,
// which every plain-text-to-HTML path in this codebase goes through. So it
// stays provider-agnostic and adds no runtime machinery, and it is advertised
// wherever the reply lands somewhere that draws it: the tool-enabled Q&A path
// and the autonomous agent runner alike.
//
// It is advertised only where the model can actually gather data, because the
// one rule that matters more than any formatting is that a chart is drawn
// from numbers the model was given, never from numbers it made up.

// ChartCapabilityPrompt is the system-prompt snippet describing the chart block.
// A package-level function (not a const) so callers read it like the other
// prompt builders and it stays trivially unit-testable. Exported for the agent
// runner, which assembles its own system prompt.
func ChartCapabilityPrompt() string {
	return "\n\n## Charts\n" +
		"When your answer is better shown as a chart (a trend over time, a breakdown by category, a " +
		"comparison of counts), you MAY render one inline by adding a fenced code block tagged `chart` " +
		"whose body is a compact JSON spec:\n" +
		"```chart\n" +
		"{\"type\":\"bar\",\"title\":\"Optional title\",\"labels\":[\"Jan\",\"Feb\"],\"series\":[{\"name\":\"Revenue\",\"values\":[10,20]}]}\n" +
		"```\n" +
		"- `type` is one of bar, line, area, pie (pie uses a single series).\n" +
		"- `labels` are the x-axis categories; each series `values` array lines up with `labels` position-by-position.\n" +
		"- Use ONLY real numbers you obtained from the context or your tools — never invent data to fill a chart.\n" +
		"- Keep it small (a chart summarizes; it is not a data dump) and still give a short written takeaway.\n" +
		"- Omit the chart when a sentence or a short list communicates the answer better."
}

// htmlArtifactCapabilityPrompt teaches the assistant that it may produce an
// INTERACTIVE artifact — a self-contained HTML/CSS/JS snippet — by emitting a
// fenced ```html block, which the frontend renders as a click-to-run, strictly
// sandboxed preview (MarkdownMessage -> AgentHtmlArtifact). Like the chart
// block it is a pure prompt contribution (the backend never parses it). Unlike
// the chart block it is advertised ONLY on the MarkdownMessage-rendered Q&A
// path, never to the agent runner: a channel post is rich text with no sandbox
// to run a document in, so advertising it there would produce code nothing
// draws.
func htmlArtifactCapabilityPrompt() string {
	return "\n\n## Interactive artifacts\n" +
		"When the user asks for something interactive or visual that a static answer can't convey — a " +
		"small prototype, a diagram, an interactive widget, a formatted layout, an animation — you MAY " +
		"produce it as a fenced code block tagged `html` containing ONE self-contained HTML document:\n" +
		"```html\n" +
		"<!doctype html><html><body> …inline CSS/JS… </body></html>\n" +
		"```\n" +
		"- Make it fully self-contained: inline all CSS and JS. Do NOT rely on external network resources " +
		"(scripts, fonts, or images from other sites) — the preview runs in an isolated sandbox with no " +
		"access to the page, cookies, storage, or your session, and external requests may be blocked.\n" +
		"- The user sees the code first and clicks Run to render it; keep it focused and reasonably small.\n" +
		"- Use it only when it genuinely helps — prefer a chart for data, and plain prose for a plain answer."
}
