package business

// Board AI: turns a natural-language prompt into a validated diagram graph
// (nodes + edges) with computed coordinates, ready for the frontend to render
// as editable Excalidraw elements and stream onto the collaborative board.
//
// The model is constrained to emit STRICT JSON (no prose), limited to a small
// set of shapes and a capped node/edge count. The server validates, sanitizes,
// de-duplicates, drops dangling edges, and runs a deterministic auto-layout so
// the output is always a clean, non-overlapping diagram - never partial or
// corrupt (atomic: either a valid graph is returned or an error).
//
// Layout is dependency-free (no dagre/elk): a layered/columnar/sequential/
// radial/stacked strategy is chosen by diagram type. This keeps the binary lean
// and the result reproducible.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// Supported diagram types. Unknown / empty values fall back to "flow".
const (
	BoardDiagramFlow      = "flow"
	BoardDiagramRoadmap   = "roadmap"
	BoardDiagramJourney   = "journey"
	BoardDiagramMindmap   = "mindmap"
	BoardDiagramOrgChart  = "orgchart"
	BoardDiagramWireframe = "wireframe"
	BoardDiagramUIMobile  = "ui-mobile"
	BoardDiagramUIDesktop = "ui-desktop"
)

var boardDiagramTypes = map[string]bool{
	BoardDiagramFlow:      true,
	BoardDiagramRoadmap:   true,
	BoardDiagramJourney:   true,
	BoardDiagramMindmap:   true,
	BoardDiagramOrgChart:  true,
	BoardDiagramWireframe: true,
	BoardDiagramUIMobile:  true,
	BoardDiagramUIDesktop: true,
}

// isUIType reports whether a diagram type is a device UI mockup (laid out as a
// device frame with stacked UI components) rather than a node/edge graph.
func isUIType(t string) bool {
	return t == BoardDiagramUIMobile || t == BoardDiagramUIDesktop
}

// Validation / safety limits.
const (
	boardMaxPromptLen = 2000
	boardMaxNodes     = 60
	boardMaxEdges     = 120
	boardMaxLabelLen  = 120
)

// boardGraphRaw is the strict shape the model must emit.
type boardGraphRaw struct {
	Title string         `json:"title"`
	Nodes []boardRawNode `json:"nodes"`
	Edges []boardRawEdge `json:"edges"`
}

type boardRawNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Shape string `json:"shape"`
	Group string `json:"group"`
}

type boardRawEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
}

// BoardLaidNode is a node with computed geometry returned to the client.
type BoardLaidNode struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Shape   string  `json:"shape"` // rectangle | ellipse | diamond
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	W       float64 `json:"w"`
	H       float64 `json:"h"`
	BgColor string  `json:"bgColor"`
}

// BoardLaidEdge connects two laid-out nodes by id.
type BoardLaidEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// BoardGenerateResult is the full validated, laid-out diagram.
type BoardGenerateResult struct {
	Title string          `json:"title"`
	Type  string          `json:"type"`
	Nodes []BoardLaidNode `json:"nodes"`
	Edges []BoardLaidEdge `json:"edges"`
	// UI mockup mode (ui-mobile / ui-desktop): a device frame + laid-out UI
	// components. Empty for graph diagram types.
	Device     string               `json:"device,omitempty"`
	Frames     []BoardLaidFrame     `json:"frames,omitempty"`
	Components []BoardLaidComponent `json:"components,omitempty"`
}

// BoardLaidFrame is a device/screen outline that components are placed inside.
type BoardLaidFrame struct {
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

// BoardLaidComponent is a fully positioned UI primitive the client renders as a
// single Excalidraw element (role + variant decide styling).
type BoardLaidComponent struct {
	Role    string  `json:"role"` // device|navbar|bar|button|input|image|card|divider|avatar|text
	Variant string  `json:"variant,omitempty"`
	Text    string  `json:"text,omitempty"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	W       float64 `json:"w"`
	H       float64 `json:"h"`
}

// Node box sizing + spacing (canvas units).
const (
	nodeW   = 200.0
	nodeH   = 80.0
	hGap    = 80.0
	vGap    = 120.0
	originX = 120.0
	originY = 120.0
)

// GenerateBoardDiagram is the entry point used by the controller after it has
// verified the caller's edit access to the board. It is resilient (rate limit +
// per-user model + circuit breaker) and never returns a partial graph.
//
// When detailed is true (and the diagram is a node/edge graph), generation runs
// a two-pass pipeline: a PLAN pass produces the major structure, then an EXPAND
// pass breaks each area into concrete sub-steps, decisions, and edge-cases. This
// yields far deeper, real-world diagrams than a single shot. detailed is ignored
// for UI mockups (they route to the design studio).
func GenerateBoardDiagram(ctx context.Context, userUUID, prompt, diagramType string, detailed bool) (*BoardGenerateResult, error) {
	return GenerateBoardDiagramWithProgress(ctx, userUUID, prompt, diagramType, detailed, nil)
}

// emitStage invokes a progress callback if present (no-op for the plain entry
// point). Stages are short, human-facing labels the streaming UI shows in order.
func emitStage(onStage func(string), stage string) {
	if onStage != nil {
		onStage(stage)
	}
}

// GenerateBoardDiagramWithProgress is GenerateBoardDiagram with a progress
// callback so the SSE endpoint can stream the pipeline's stages
// (understanding -> planning -> expanding -> laying out) to the user.
func GenerateBoardDiagramWithProgress(ctx context.Context, userUUID, prompt, diagramType string, detailed bool, onStage func(string)) (*BoardGenerateResult, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}

	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("a prompt is required")
	}
	if len(prompt) > boardMaxPromptLen {
		prompt = prompt[:boardMaxPromptLen]
	}

	diagramType = strings.ToLower(strings.TrimSpace(diagramType))
	explicitType := boardDiagramTypes[diagramType]

	// Rate limit per user (human is waiting).
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}

	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}

	// Auto mode: when the caller did not force a specific diagram type (the
	// default in the composer), let the model classify the request so the right
	// layout AND output schema (node/edge graph vs UI mockup) is chosen without
	// the user having to pick. Any failure falls back to a flowchart.
	if !explicitType {
		emitStage(onStage, "understanding")
		diagramType = classifyDiagramType(ctx, llm, prompt)
	}

	// UI mockup mode (device frame + components): a single structured call.
	if isUIType(diagramType) {
		emitStage(onStage, "designing")
		opts := ai.ChatOptions{Temperature: 0.2, MaxTokens: 2048, LowLatency: true, JSONMode: true}
		messages := []ai.ChatMessage{
			{Role: "system", Content: boardSystemPrompt(diagramType)},
			{Role: "user", Content: prompt},
		}
		out, err := ai.ChatWithRescue(ctx, llm, messages, opts)
		if err != nil {
			cb.RecordResult(err)
			helpers.LogErrorWithContext(ctx, "business/GenerateBoardDiagram LLM failed: %+v", err)
			return nil, fmt.Errorf("the AI could not generate a diagram right now")
		}
		cb.RecordSuccess()
		mockup, perr := parseUIMockup(out)
		if perr != nil {
			helpers.LogErrorWithContext(ctx, "business/GenerateBoardDiagram UI parse failed: %+v raw=%q", perr, truncateForLog(out))
			return nil, fmt.Errorf("the AI returned an unexpected response, please try again")
		}
		result := layoutUIMockup(mockup, diagramType)
		if len(result.Components) == 0 {
			return nil, fmt.Errorf("the AI could not produce a mockup for that prompt, try rephrasing")
		}
		return result, nil
	}

	// Node/edge graph mode: one pass (quick) or plan -> expand (detailed).
	graph, err := produceBoardGraph(ctx, llm, cb, prompt, diagramType, detailed, onStage)
	if err != nil {
		return nil, err
	}

	emitStage(onStage, "laying out")
	result := layoutBoardGraph(graph, diagramType)
	if len(result.Nodes) == 0 {
		return nil, fmt.Errorf("the AI could not produce a diagram for that prompt, try rephrasing")
	}
	return result, nil
}

// produceBoardGraph runs the model to obtain a validated raw graph. In detailed
// mode it first asks for a high-level plan, then expands that plan into a deep
// graph; if either deep pass fails it degrades gracefully (expand failure keeps
// the plan; plan failure falls back to a single direct pass) so the user always
// gets a usable diagram.
func produceBoardGraph(ctx context.Context, llm ai.LLMProvider, cb *ai.CircuitBreaker, prompt, diagramType string, detailed bool, onStage func(string)) (*boardGraphRaw, error) {
	if detailed {
		emitStage(onStage, "planning")
		planOpts := ai.ChatOptions{Temperature: 0.2, MaxTokens: 1200, JSONMode: true}
		plan, planErr := chatBoardGraph(ctx, llm, cb, boardPlanSystemPrompt(diagramType), prompt, planOpts)
		if planErr == nil && plan != nil && len(plan.Nodes) > 0 {
			emitStage(onStage, "expanding")
			planJSON := marshalGraphForPrompt(plan)
			expandOpts := ai.ChatOptions{Temperature: 0.25, MaxTokens: 4096, JSONMode: true}
			expandUser := "Original request:\n" + prompt + "\n\nApproved outline (expand every node into real depth):\n" + planJSON
			expanded, expErr := chatBoardGraph(ctx, llm, cb, boardExpandSystemPrompt(diagramType), expandUser, expandOpts)
			if expErr == nil && expanded != nil && len(expanded.Nodes) >= len(plan.Nodes) {
				return expanded, nil
			}
			// Expansion failed or regressed: the plan itself is a valid diagram.
			return plan, nil
		}
		// Plan failed: fall through to the single-pass path below.
	}

	emitStage(onStage, "drafting")
	opts := ai.ChatOptions{Temperature: 0.2, MaxTokens: 2048, LowLatency: true, JSONMode: true}
	graph, err := chatBoardGraph(ctx, llm, cb, boardSystemPrompt(diagramType), prompt, opts)
	if err != nil {
		return nil, fmt.Errorf("the AI returned an unexpected response, please try again")
	}
	return graph, nil
}

// chatBoardGraph performs one model call (plus one corrective retry on a parse
// failure) and returns the validated raw graph. Records circuit-breaker
// outcome. Shared by the plan, expand, single-pass, and refine flows.
func chatBoardGraph(ctx context.Context, llm ai.LLMProvider, cb *ai.CircuitBreaker, system, user string, opts ai.ChatOptions) (*boardGraphRaw, error) {
	messages := []ai.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	out, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "business/chatBoardGraph LLM failed: %+v", err)
		return nil, err
	}
	cb.RecordSuccess()

	graph, perr := parseBoardGraph(out)
	if perr == nil {
		return graph, nil
	}
	// One corrective retry: small models occasionally wrap the JSON in prose or
	// emit a trailing explanation.
	retry := []ai.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
		{Role: "assistant", Content: out},
		{Role: "user", Content: "That was not valid. Reply with ONLY the JSON object described in the schema. No prose, no code fences."},
	}
	if retryOut, rerr := ai.ChatWithRescue(ctx, llm, retry, opts); rerr == nil {
		if g2, p2 := parseBoardGraph(retryOut); p2 == nil {
			return g2, nil
		}
	}
	helpers.LogErrorWithContext(ctx, "business/chatBoardGraph parse failed: %+v raw=%q", perr, truncateForLog(out))
	return nil, perr
}

// RefineBoardDiagram applies a natural-language change to an existing diagram
// and returns the full, re-validated, re-laid-out graph. The current graph is
// given to the model; it preserves the ids of nodes that remain, so the change
// reads as an edit rather than a regeneration. Resilient + edit-access gated by
// the controller, same as generation.
func RefineBoardDiagram(ctx context.Context, userUUID, instruction, diagramType string, current *BoardGenerateResult) (*BoardGenerateResult, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return nil, fmt.Errorf("describe the change you want")
	}
	if len(instruction) > boardMaxPromptLen {
		instruction = instruction[:boardMaxPromptLen]
	}
	if current == nil || len(current.Nodes) == 0 {
		return nil, fmt.Errorf("no diagram to refine")
	}

	diagramType = strings.ToLower(strings.TrimSpace(diagramType))
	if !boardDiagramTypes[diagramType] || isUIType(diagramType) {
		diagramType = BoardDiagramFlow
	}

	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}

	currentJSON := marshalLaidGraphForPrompt(current)
	user := "Current diagram:\n" + currentJSON + "\n\nChange to apply:\n" + instruction
	opts := ai.ChatOptions{Temperature: 0.2, MaxTokens: 4096, JSONMode: true}

	graph, err := chatBoardGraph(ctx, llm, cb, boardRefineSystemPrompt(diagramType), user, opts)
	if err != nil {
		return nil, fmt.Errorf("the AI could not apply that change, please try again")
	}
	result := layoutBoardGraph(graph, diagramType)
	if len(result.Nodes) == 0 {
		return nil, fmt.Errorf("the change produced an empty diagram, try rephrasing")
	}
	return result, nil
}

// marshalGraphForPrompt serializes a raw plan graph compactly for the expand
// prompt (ids + labels + shapes + edges only).
func marshalGraphForPrompt(g *boardGraphRaw) string {
	b, err := json.Marshal(g)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// marshalLaidGraphForPrompt serializes the current laid-out graph back to the
// minimal {title,nodes,edges} schema the model edits (drops geometry/colour).
func marshalLaidGraphForPrompt(r *BoardGenerateResult) string {
	raw := boardGraphRaw{Title: r.Title}
	for _, n := range r.Nodes {
		raw.Nodes = append(raw.Nodes, boardRawNode{ID: n.ID, Label: n.Label, Shape: n.Shape})
	}
	for _, e := range r.Edges {
		raw.Edges = append(raw.Edges, boardRawEdge{From: e.From, To: e.To, Label: e.Label})
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// truncateForLog caps a model response for a diagnostic log line so a failed
// parse is debuggable without flooding the logs with a full payload.
func truncateForLog(s string) string {
	s = strings.TrimSpace(s)
	const max = 400
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// classifyDiagramType maps a free-form request to one of the supported diagram
// types so "Auto" mode picks the right layout and output schema for the user.
// It is a tiny, fast call; any error or unrecognized answer falls back to a
// flowchart so generation always proceeds.
func classifyDiagramType(ctx context.Context, llm ai.LLMProvider, prompt string) string {
	sys := strings.Join([]string{
		"Classify the user's whiteboard request into exactly ONE category.",
		"Reply with ONLY the category id in lowercase, nothing else.",
		"Categories:",
		"- flow: a process or flowchart with steps and decisions",
		"- roadmap: initiatives across phases or a timeline",
		"- journey: a user or customer journey across ordered stages",
		"- mindmap: a central idea with branching subtopics",
		"- orgchart: a hierarchy of people, roles, or teams",
		"- wireframe: a rough page layout of stacked sections",
		"- ui-mobile: a mobile app screen or UI mockup",
		"- ui-desktop: a desktop or web app screen or UI mockup",
		"If unsure, reply: flow",
	}, "\n")
	messages := []ai.ChatMessage{
		{Role: "system", Content: sys},
		{Role: "user", Content: prompt},
	}
	out, err := ai.ChatWithRescue(ctx, llm, messages, ai.ChatOptions{Temperature: 0, MaxTokens: 8, LowLatency: true})
	if err != nil {
		return BoardDiagramFlow
	}
	out = strings.ToLower(out)
	// Check the more specific ids (ui-*) before the generic ones so a response
	// like "ui-mobile" is not shadowed by a substring match.
	for _, t := range []string{
		BoardDiagramUIMobile, BoardDiagramUIDesktop, BoardDiagramOrgChart,
		BoardDiagramRoadmap, BoardDiagramJourney, BoardDiagramMindmap,
		BoardDiagramWireframe, BoardDiagramFlow,
	} {
		if strings.Contains(out, t) {
			return t
		}
	}
	return BoardDiagramFlow
}

// boardSystemPrompt builds a strict-JSON instruction tuned per diagram type.
func boardSystemPrompt(diagramType string) string {
	if isUIType(diagramType) {
		return boardUISystemPrompt(diagramType)
	}
	var guidance string
	switch diagramType {
	case BoardDiagramRoadmap:
		guidance = "Produce a product roadmap. Use \"group\" for the phase or timeframe (e.g. \"Now\", \"Next\", \"Later\", or quarter names). Each node is an initiative. Use rectangle shapes. Edges are optional dependencies."
	case BoardDiagramJourney:
		guidance = "Produce a user journey as an ordered sequence of stages from left to right. Each node is a stage. Connect consecutive stages with edges. Use rectangle shapes."
	case BoardDiagramMindmap:
		guidance = "Produce a mind map with ONE central root node and branches radiating out. The first node is the root (use an ellipse). Connect the root to its main branches and branches to sub-topics with edges."
	case BoardDiagramOrgChart:
		guidance = "Produce an organization chart as a top-down hierarchy. The top node is the most senior role. Each edge goes from a manager to a direct report. Use rectangle shapes."
	case BoardDiagramWireframe:
		guidance = "Produce a simple UI wireframe as a vertical stack of sections (header, content blocks, footer). Each node is a section labelled with its purpose. Use rectangle shapes. Edges are not required."
	default: // flow
		guidance = "Produce a process flowchart. Use ellipse for start/end nodes, diamond for decisions, and rectangle for steps. Connect nodes with directed edges; label decision branches (e.g. \"Yes\"/\"No\")."
	}

	return strings.Join([]string{
		"You are a diagramming assistant for a collaborative whiteboard.",
		"Convert the user's request into a diagram described as STRICT JSON only.",
		guidance,
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- Schema: {\"title\": string, \"nodes\": [{\"id\": string, \"label\": string, \"shape\": \"rectangle\"|\"ellipse\"|\"diamond\", \"group\": string}], \"edges\": [{\"from\": string, \"to\": string, \"label\": string}]}.",
		"- \"id\" is a short unique slug (e.g. \"n1\"). \"from\"/\"to\" reference node ids.",
		"- Keep labels concise (a few words). Use at most 40 nodes.",
		"- \"group\" and edge \"label\" may be empty strings when not applicable.",
		"- Every edge must reference ids that exist in nodes.",
	}, "\n")
}

// boardGraphSchemaLine is the shared schema description so the plan, expand,
// and refine prompts stay consistent with parseBoardGraph / validateGraph.
const boardGraphSchemaLine = "Schema: {\"title\": string, \"nodes\": [{\"id\": string, \"label\": string, \"shape\": \"rectangle\"|\"ellipse\"|\"diamond\", \"group\": string}], \"edges\": [{\"from\": string, \"to\": string, \"label\": string}]}."

// boardPlanSystemPrompt asks for a high-level OUTLINE: the major areas/phases/
// branches only, which the expand pass then deepens. Few nodes, broad coverage.
func boardPlanSystemPrompt(diagramType string) string {
	return strings.Join([]string{
		"You are a senior systems analyst planning a " + diagramTypeNoun(diagramType) + " for a collaborative whiteboard.",
		"Produce a HIGH-LEVEL OUTLINE only: the major sections / phases / branches a complete, real-world diagram of this request would contain. Aim for 5 to 12 outline nodes that comprehensively cover the topic end to end. Do NOT detail sub-steps yet.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- " + boardGraphSchemaLine,
		"- Cover the whole topic at a high level: setup/entry, the main stages, key decision areas, and completion/exit. Think about what an expert would not want missing.",
		"- Connect the outline nodes with edges to show the overall flow or hierarchy.",
		"- Keep labels concise. \"group\" and edge \"label\" may be empty strings.",
	}, "\n")
}

// boardExpandSystemPrompt deepens an approved outline into a complete, detailed
// diagram. This is where real-world depth comes from.
func boardExpandSystemPrompt(diagramType string) string {
	return strings.Join([]string{
		"You are a senior systems analyst turning an approved outline into a COMPLETE, DEEP " + diagramTypeNoun(diagramType) + ".",
		"Expand EVERY outline node into concrete sub-steps. Add the detail an expert would expect: real decision points (with labelled Yes/No or condition branches), error and edge cases, validation, retries, hand-offs, and exit/completion states. Preserve the outline's overall structure and ordering.",
		"",
		"Depth rules:",
		"- Break each outline node into 2 to 6 concrete child nodes; do not leave any outline node un-expanded.",
		"- Use the right shapes: ellipse for start/end, diamond for decisions, rectangle for steps.",
		"- Label decision edges with the condition (e.g. \"Yes\", \"No\", \"Payment failed\").",
		"- Be realistic and specific to the request, not generic. Use up to 60 nodes to capture genuine depth.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- " + boardGraphSchemaLine,
		"- Every edge must reference ids that exist in nodes. Ids are short unique slugs.",
	}, "\n")
}

// boardRefineSystemPrompt edits an existing diagram per a user instruction,
// returning the full updated graph.
func boardRefineSystemPrompt(diagramType string) string {
	return strings.Join([]string{
		"You are editing an existing " + diagramTypeNoun(diagramType) + " on a collaborative whiteboard.",
		"Apply the user's requested change to the diagram given below, then return the COMPLETE updated diagram (not just the change).",
		"",
		"Editing rules:",
		"- PRESERVE the \"id\" of every node you keep, so unchanged parts stay stable. Add new nodes with new unique ids; remove nodes the change makes obsolete; update labels/shapes as needed.",
		"- Keep the diagram coherent and fully connected: every edge must reference ids that exist in nodes.",
		"- Make only the change requested (and what it directly implies); do not rebuild unrelated parts.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- " + boardGraphSchemaLine,
	}, "\n")
}

// diagramTypeNoun returns a human noun for a diagram type, for prompt phrasing.
func diagramTypeNoun(t string) string {
	switch t {
	case BoardDiagramRoadmap:
		return "product roadmap"
	case BoardDiagramJourney:
		return "user journey"
	case BoardDiagramMindmap:
		return "mind map"
	case BoardDiagramOrgChart:
		return "org chart"
	case BoardDiagramWireframe:
		return "wireframe"
	default:
		return "flowchart"
	}
}

// parseBoardGraph extracts and validates the model's JSON object.
func parseBoardGraph(raw string) (*boardGraphRaw, error) {
	js := extractJSONObject(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var g boardGraphRaw
	if err := json.Unmarshal([]byte(js), &g); err != nil {
		return nil, err
	}
	if len(g.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes")
	}
	return &g, nil
}

// extractJSONObject returns the first top-level JSON object substring, tolerant
// of code fences and surrounding prose.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// sanitizeLabel trims, collapses whitespace, strips control characters, and
// caps the length so a hostile or sloppy model output can't break rendering.
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if r < 32 {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > boardMaxLabelLen {
		s = s[:boardMaxLabelLen]
	}
	return s
}

func normalizeShape(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ellipse", "circle", "oval":
		return "ellipse"
	case "diamond", "decision", "rhombus":
		return "diamond"
	default:
		return "rectangle"
	}
}

// bgForShape returns a soft, Notion-like pastel background per shape.
func bgForShape(shape string) string {
	switch shape {
	case "ellipse":
		return "#ebfbee" // light green (start/end)
	case "diamond":
		return "#fff9db" // light yellow (decision)
	default:
		return "#e7f5ff" // light blue (step)
	}
}

// validatedGraph holds the cleaned node set + edge set after validation.
type validatedGraph struct {
	title  string
	order  []string // node ids in original order
	nodes  map[string]boardRawNode
	groups []string // unique group order
	edges  []boardRawEdge
}

func validateGraph(g *boardGraphRaw) validatedGraph {
	v := validatedGraph{
		title: sanitizeLabel(g.Title),
		nodes: make(map[string]boardRawNode),
	}
	seenGroup := map[string]bool{}
	for _, n := range g.Nodes {
		id := strings.TrimSpace(n.ID)
		if id == "" {
			continue
		}
		if _, dup := v.nodes[id]; dup {
			continue
		}
		label := sanitizeLabel(n.Label)
		if label == "" {
			label = id
		}
		grp := sanitizeLabel(n.Group)
		clean := boardRawNode{ID: id, Label: label, Shape: normalizeShape(n.Shape), Group: grp}
		v.nodes[id] = clean
		v.order = append(v.order, id)
		if grp != "" && !seenGroup[grp] {
			seenGroup[grp] = true
			v.groups = append(v.groups, grp)
		}
		if len(v.order) >= boardMaxNodes {
			break
		}
	}
	// Resolve edge endpoints tolerantly. Small models often reference a node by
	// its label, a case/space variant, or a near-miss id; strict exact-id
	// matching silently drops those edges, leaving a diagram with missing
	// connector lines. Resolve a reference to a node id by: exact id, then
	// case-insensitive id, then exact label, then case-insensitive label.
	idByLowerID := make(map[string]string, len(v.nodes))
	idByLabel := make(map[string]string, len(v.nodes))
	for id, n := range v.nodes {
		idByLowerID[strings.ToLower(id)] = id
		if n.Label != "" {
			ll := strings.ToLower(n.Label)
			if _, exists := idByLabel[ll]; !exists {
				idByLabel[ll] = id
			}
		}
	}
	resolveRef := func(ref string) string {
		r := strings.TrimSpace(ref)
		if r == "" {
			return ""
		}
		if _, ok := v.nodes[r]; ok {
			return r
		}
		if id, ok := idByLowerID[strings.ToLower(r)]; ok {
			return id
		}
		if id, ok := idByLabel[strings.ToLower(r)]; ok {
			return id
		}
		return ""
	}

	seenEdge := map[string]bool{}
	for _, e := range g.Edges {
		from := resolveRef(e.From)
		to := resolveRef(e.To)
		if from == "" || to == "" || from == to {
			continue
		}
		dedupeKey := from + "\x00" + to
		if seenEdge[dedupeKey] {
			continue
		}
		seenEdge[dedupeKey] = true
		v.edges = append(v.edges, boardRawEdge{From: from, To: to, Label: sanitizeLabel(e.Label)})
		if len(v.edges) >= boardMaxEdges {
			break
		}
	}
	return v
}

// layoutBoardGraph validates then assigns coordinates per diagram type.
func layoutBoardGraph(g *boardGraphRaw, diagramType string) *BoardGenerateResult {
	v := validateGraph(g)

	// Guarantee connector lines when the model returned nodes but no usable
	// edges (a common small-model miss, especially after tolerant resolution
	// still finds nothing). Synthesize a sensible default per diagram type so a
	// generated diagram is never a set of disconnected boxes:
	//   - mind map / org chart: spokes from the first node (root / most senior)
	//   - flow / journey: a linear chain through the nodes in order
	// Roadmap (grouped columns) and wireframe (stacked sections) do not need edges.
	if len(v.edges) == 0 && len(v.order) > 1 {
		switch diagramType {
		case BoardDiagramMindmap, BoardDiagramOrgChart:
			root := v.order[0]
			for _, id := range v.order[1:] {
				v.edges = append(v.edges, boardRawEdge{From: root, To: id})
			}
		case BoardDiagramFlow, BoardDiagramJourney:
			for i := 1; i < len(v.order); i++ {
				v.edges = append(v.edges, boardRawEdge{From: v.order[i-1], To: v.order[i]})
			}
		}
	}

	res := &BoardGenerateResult{Title: v.title, Type: diagramType}
	if res.Title == "" {
		res.Title = "Untitled diagram"
	}

	var positions map[string]point
	switch diagramType {
	case BoardDiagramJourney:
		positions = layoutSequential(v)
	case BoardDiagramRoadmap:
		positions = layoutColumnsByGroup(v)
	case BoardDiagramMindmap:
		positions = layoutRadial(v)
	case BoardDiagramWireframe:
		positions = layoutStacked(v)
	default: // flow, orgchart
		positions = layoutLayered(v)
	}

	for _, id := range v.order {
		n := v.nodes[id]
		p, ok := positions[id]
		if !ok {
			continue
		}
		res.Nodes = append(res.Nodes, BoardLaidNode{
			ID: id, Label: n.Label, Shape: n.Shape,
			X: p.x, Y: p.y, W: nodeW, H: nodeH, BgColor: bgForShape(n.Shape),
		})
	}
	for _, e := range v.edges {
		res.Edges = append(res.Edges, BoardLaidEdge{From: e.From, To: e.To, Label: e.Label})
	}
	return res
}

type point struct{ x, y float64 }

// layoutLayered places nodes top-down in layers using BFS depth from roots
// (nodes with no incoming edge). Used for flow + orgchart.
func layoutLayered(v validatedGraph) map[string]point {
	incoming := map[string]int{}
	adj := map[string][]string{}
	for _, id := range v.order {
		incoming[id] = 0
	}
	for _, e := range v.edges {
		incoming[e.To]++
		adj[e.From] = append(adj[e.From], e.To)
	}

	// Roots: no incoming. If none (cycle), use the first node.
	var roots []string
	for _, id := range v.order {
		if incoming[id] == 0 {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 && len(v.order) > 0 {
		roots = []string{v.order[0]}
	}

	depth := map[string]int{}
	for _, r := range roots {
		depth[r] = 0
	}
	queue := append([]string{}, roots...)
	visited := map[string]bool{}
	for _, r := range roots {
		visited[r] = true
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range adj[cur] {
			if d := depth[cur] + 1; !visited[nb] || depth[nb] < d {
				if d > depth[nb] {
					depth[nb] = d
				}
			}
			if !visited[nb] {
				visited[nb] = true
				queue = append(queue, nb)
			}
		}
	}
	// Any node never reached (disconnected) goes to its own depth bucket.
	for _, id := range v.order {
		if !visited[id] {
			depth[id] = 0
		}
	}

	// Group node ids by depth, preserving original order.
	layers := map[int][]string{}
	maxDepth := 0
	for _, id := range v.order {
		d := depth[id]
		layers[d] = append(layers[d], id)
		if d > maxDepth {
			maxDepth = d
		}
	}

	positions := map[string]point{}
	for d := 0; d <= maxDepth; d++ {
		row := layers[d]
		rowWidth := float64(len(row))*nodeW + float64(len(row)-1)*hGap
		startX := originX - rowWidth/2 + nodeW/2
		if len(row) == 1 {
			startX = originX
		}
		for i, id := range row {
			positions[id] = point{
				x: startX + float64(i)*(nodeW+hGap),
				y: originY + float64(d)*(nodeH+vGap),
			}
		}
	}
	return positions
}

// layoutSequential lays nodes left-to-right in a single row (user journey).
func layoutSequential(v validatedGraph) map[string]point {
	positions := map[string]point{}
	for i, id := range v.order {
		positions[id] = point{x: originX + float64(i)*(nodeW+hGap), y: originY}
	}
	return positions
}

// layoutColumnsByGroup makes a column per group (roadmap phases) and stacks the
// group's nodes vertically. Ungrouped nodes share a trailing column.
func layoutColumnsByGroup(v validatedGraph) map[string]point {
	groups := v.groups
	colOf := map[string]int{}
	for i, g := range groups {
		colOf[g] = i
	}
	ungroupedCol := len(groups)

	rowInCol := map[int]int{}
	positions := map[string]point{}
	for _, id := range v.order {
		n := v.nodes[id]
		col := ungroupedCol
		if n.Group != "" {
			col = colOf[n.Group]
		}
		r := rowInCol[col]
		rowInCol[col] = r + 1
		positions[id] = point{
			x: originX + float64(col)*(nodeW+hGap),
			y: originY + nodeH + float64(r)*(nodeH+vGap/2),
		}
	}
	return positions
}

// layoutStacked stacks nodes vertically in one column (wireframe sections).
func layoutStacked(v validatedGraph) map[string]point {
	positions := map[string]point{}
	for i, id := range v.order {
		positions[id] = point{x: originX, y: originY + float64(i)*(nodeH+vGap/3)}
	}
	return positions
}

// layoutRadial puts the first node at the centre and the rest evenly around it
// (mind map). Deeper structure is approximated by ringing remaining nodes.
func layoutRadial(v validatedGraph) map[string]point {
	positions := map[string]point{}
	if len(v.order) == 0 {
		return positions
	}
	cx, cy := 600.0, 400.0
	root := v.order[0]
	positions[root] = point{x: cx - nodeW/2, y: cy - nodeH/2}

	rest := v.order[1:]
	if len(rest) == 0 {
		return positions
	}
	radius := 280.0
	if len(rest) > 8 {
		radius = 280.0 + float64(len(rest)-8)*18.0
	}
	for i, id := range rest {
		angle := 2 * math.Pi * float64(i) / float64(len(rest))
		positions[id] = point{
			x: cx + radius*math.Cos(angle) - nodeW/2,
			y: cy + radius*math.Sin(angle) - nodeH/2,
		}
	}
	return positions
}
