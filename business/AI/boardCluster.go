package business

// Board clustering / synthesis — Miro parity ("cluster the stickies").
//
// Given the free-text already on a board (sticky notes, text, labeled shapes),
// ClusterBoardItems asks the model to group them into a handful of named themes
// and write a short synthesis. The canvas is never mutated; instead the result
// is turned into a laid-out summary MIND MAP (reusing the mature board layout)
// that the client inserts beside the originals, plus the structured themes for
// a text summary. This is the brainstorm-to-structure step a whiteboard needs
// after a divergent session.
//
// Properties (shared with the rest of board AI):
//   - Provider-agnostic: runs through the AI service chokepoint + per-user
//     model resolution; no vendor coupling.
//   - Resilient: rate limit + circuit breaker, one corrective retry on a bad
//     parse, graceful errors.
//   - Bounded + safe: item count, per-item text, and theme count are capped;
//     the model emits STRICT JSON which is validated and sanitized; item ids
//     are constrained to the set the client actually sent (a theme can never
//     reference content that wasn't provided).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	boardClusterMinItems        = 3
	boardClusterMaxItems        = 120
	boardClusterMaxThemes       = 8
	boardClusterItemTextLen     = 240
	boardClusterMaxLeafPerTheme = 6
)

// BoardClusterInput is one canvas text item (business-layer shape; the
// controller maps the adapter DTO onto this so business has no adapter dep).
type BoardClusterInput struct {
	ID   string
	Text string
}

// BoardClusterView is one theme: its label, a one-line summary, and the ids of
// the items assigned to it (a subset of what the client sent).
type BoardClusterView struct {
	Theme   string   `json:"theme"`
	Summary string   `json:"summary"`
	ItemIDs []string `json:"item_ids"`
}

// BoardClusterResult is the full clustering response.
type BoardClusterResult struct {
	Title     string             `json:"title"`
	Synthesis string             `json:"synthesis"`
	Clusters  []BoardClusterView `json:"clusters"`
	// Graph is a laid-out mind map (root -> themes -> items) ready for the
	// client to render with the same path as a generated diagram.
	Graph *BoardGenerateResult `json:"graph"`
}

// ClusterBoardItems groups the supplied canvas items into themes and returns a
// synthesis plus an insertable summary mind map. Resilient + gated; the caller
// (controller) enforces board edit access.
func ClusterBoardItems(ctx context.Context, userUUID string, items []BoardClusterInput) (*BoardClusterResult, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}

	// Sanitize + bound the input set, and build the id->text lookup the graph
	// builder uses for leaf labels.
	clean := make([]BoardClusterInput, 0, len(items))
	textByID := make(map[string]string, len(items))
	validIDs := make(map[string]bool, len(items))
	for _, it := range items {
		id := strings.TrimSpace(it.ID)
		text := sanitizeLabel(it.Text) // collapses whitespace, strips control chars
		if id == "" || text == "" || validIDs[id] {
			continue
		}
		if len([]rune(text)) > boardClusterItemTextLen {
			text = string([]rune(text)[:boardClusterItemTextLen])
		}
		validIDs[id] = true
		textByID[id] = text
		clean = append(clean, BoardClusterInput{ID: id, Text: text})
		if len(clean) >= boardClusterMaxItems {
			break
		}
	}
	if len(clean) < boardClusterMinItems {
		return nil, fmt.Errorf("add a few more notes to the board first (at least %d)", boardClusterMinItems)
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

	user := formatClusterItemsForPrompt(clean)
	parsed, err := chatBoardClusters(ctx, llm, cb, boardClusterSystemPrompt(), user, validIDs)
	if err != nil {
		return nil, fmt.Errorf("the AI could not cluster these notes right now, please try again")
	}

	result := &BoardClusterResult{
		Title:     parsed.Title,
		Synthesis: parsed.Synthesis,
		Clusters:  parsed.Clusters,
	}
	if result.Title == "" {
		result.Title = "Board synthesis"
	}
	result.Graph = buildClusterGraph(result.Title, parsed.Clusters, textByID)
	return result, nil
}

// chatBoardClusters performs one model call (plus one corrective retry on a
// parse failure) and returns the validated clusters.
func chatBoardClusters(ctx context.Context, llm ai.LLMProvider, cb *ai.CircuitBreaker, system, user string, validIDs map[string]bool) (*parsedClusters, error) {
	messages := []ai.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	out, err := ai.ChatWithRescue(ctx, llm, messages, ai.ChatOptions{Temperature: 0.2, MaxTokens: 1600, JSONMode: true})
	if err != nil {
		cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "business/chatBoardClusters LLM failed: %+v", err)
		return nil, err
	}
	cb.RecordSuccess()

	if parsed, perr := parseBoardClusters(out, validIDs); perr == nil {
		return parsed, nil
	}
	retry := []ai.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
		{Role: "assistant", Content: out},
		{Role: "user", Content: "That was not valid. Reply with ONLY the JSON object described in the schema. No prose, no code fences."},
	}
	if retryOut, rerr := ai.ChatWithRescue(ctx, llm, retry, ai.ChatOptions{Temperature: 0.2, MaxTokens: 1600, JSONMode: true}); rerr == nil {
		if parsed, p2 := parseBoardClusters(retryOut, validIDs); p2 == nil {
			return parsed, nil
		}
	}
	return nil, fmt.Errorf("could not parse clusters")
}

// parsedClusters is the validated intermediate form.
type parsedClusters struct {
	Title     string
	Synthesis string
	Clusters  []BoardClusterView
}

// boardClusterRaw is the strict JSON the model must emit.
type boardClusterRaw struct {
	Title     string `json:"title"`
	Synthesis string `json:"synthesis"`
	Clusters  []struct {
		Theme   string   `json:"theme"`
		Summary string   `json:"summary"`
		ItemIDs []string `json:"item_ids"`
	} `json:"clusters"`
}

// parseBoardClusters extracts + validates the model JSON. Pure + unit tested.
// Item ids are constrained to validIDs (so a theme can never reference content
// the client didn't send), each item is assigned to at most one theme (first
// wins), empty themes are dropped, and theme count is capped.
func parseBoardClusters(raw string, validIDs map[string]bool) (*parsedClusters, error) {
	js := extractJSONObject(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var c boardClusterRaw
	if err := json.Unmarshal([]byte(js), &c); err != nil {
		return nil, err
	}

	out := &parsedClusters{
		Title:     sanitizeLabel(c.Title),
		Synthesis: sanitizeSynthesis(c.Synthesis),
	}
	assigned := make(map[string]bool, len(validIDs))
	for _, cl := range c.Clusters {
		theme := sanitizeLabel(cl.Theme)
		if theme == "" {
			continue
		}
		ids := make([]string, 0, len(cl.ItemIDs))
		for _, id := range cl.ItemIDs {
			id = strings.TrimSpace(id)
			if id == "" || !validIDs[id] || assigned[id] {
				continue
			}
			assigned[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			continue // a theme with no real items is noise
		}
		out.Clusters = append(out.Clusters, BoardClusterView{
			Theme:   theme,
			Summary: sanitizeSynthesis(cl.Summary),
			ItemIDs: ids,
		})
		if len(out.Clusters) >= boardClusterMaxThemes {
			break
		}
	}
	if len(out.Clusters) == 0 {
		return nil, fmt.Errorf("no usable clusters")
	}
	return out, nil
}

// sanitizeSynthesis trims a free-text summary, strips control chars, and caps
// it so the response stays bounded (longer cap than a node label).
func sanitizeSynthesis(s string) string {
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
	const max = 600
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

// buildClusterGraph turns the themes into a mind map (root -> theme -> items)
// and lays it out with the existing radial layout, so the client renders it
// through the same path as a generated diagram. Leaf nodes per theme are capped
// to keep the total node count within the layout's bounds. Pure (no I/O).
func buildClusterGraph(title string, clusters []BoardClusterView, textByID map[string]string) *BoardGenerateResult {
	raw := &boardGraphRaw{Title: title}
	raw.Nodes = append(raw.Nodes, boardRawNode{ID: "root", Label: title, Shape: "ellipse"})
	for i, cl := range clusters {
		cid := fmt.Sprintf("c%d", i)
		raw.Nodes = append(raw.Nodes, boardRawNode{ID: cid, Label: cl.Theme, Shape: "rectangle"})
		raw.Edges = append(raw.Edges, boardRawEdge{From: "root", To: cid})
		for j, itemID := range cl.ItemIDs {
			if j >= boardClusterMaxLeafPerTheme {
				break
			}
			text := textByID[itemID]
			if text == "" {
				continue
			}
			liid := fmt.Sprintf("%s_i%d", cid, j)
			raw.Nodes = append(raw.Nodes, boardRawNode{ID: liid, Label: text, Shape: "rectangle"})
			raw.Edges = append(raw.Edges, boardRawEdge{From: cid, To: liid})
		}
	}
	return layoutBoardGraph(raw, BoardDiagramMindmap)
}

// formatClusterItemsForPrompt renders the items as an id-tagged list the model
// references by id when assigning themes.
func formatClusterItemsForPrompt(items []BoardClusterInput) string {
	var sb strings.Builder
	sb.WriteString("Notes on the board (group these into themes by their ids):\n")
	for _, it := range items {
		sb.WriteString(fmt.Sprintf("[%s] %s\n", it.ID, it.Text))
	}
	return sb.String()
}

// boardClusterSystemPrompt instructs the model to theme + synthesize in strict
// JSON, referencing only the supplied ids.
func boardClusterSystemPrompt() string {
	return strings.Join([]string{
		"You are a facilitation assistant for a collaborative whiteboard. The user gives a flat list of sticky notes / text items, each tagged with an id.",
		"Group the items into a small number of meaningful THEMES (aim for 2 to 6, never more than 8). Give each theme a short title and a one-line summary, and list the ids of the items that belong to it. Then write a brief overall synthesis of the whole board.",
		"",
		"Rules:",
		"- Use ONLY the ids provided. Never invent ids or items.",
		"- Put each item in the single theme that fits best; do not duplicate an item across themes. It is fine to leave a stray item out.",
		"- Base themes and the synthesis ONLY on the given text. Do not invent content.",
		"- Keep theme titles to a few words and summaries to one concise line.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- Schema: {\"title\": string, \"synthesis\": string, \"clusters\": [{\"theme\": string, \"summary\": string, \"item_ids\": [string]}]}.",
	}, "\n")
}
