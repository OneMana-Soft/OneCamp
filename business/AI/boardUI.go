package business

// Board AI - UI/UX mockup mode. Turns a prompt into a device-accurate desktop
// or mobile screen mockup: the model emits a semantic screen description
// (ordered blocks of UI components) and the SERVER lays it out into a device
// frame with a deterministic vertical-stack engine. Doing layout server-side
// (instead of trusting the model to place pixels) guarantees clean,
// non-overlapping, on-grid mockups every time.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// uiMockupRaw is the strict shape the model must emit for UI types.
type uiMockupRaw struct {
	Title   string        `json:"title"`
	Screens []uiScreenRaw `json:"screens"`
	// Some models return a single screen's blocks at the top level; accept that
	// too so we never fail on a reasonable response.
	Blocks []uiBlockRaw `json:"blocks"`
}

type uiScreenRaw struct {
	Name   string       `json:"name"`
	Blocks []uiBlockRaw `json:"blocks"`
}

type uiBlockRaw struct {
	Type    string `json:"type"`
	Label   string `json:"label"`
	Variant string `json:"variant"`
}

// UI layout limits.
const (
	uiMaxScreens       = 4
	uiMaxBlocksPerScrn = 30
)

// boardUISystemPrompt instructs the model to emit a strict-JSON screen spec.
func boardUISystemPrompt(diagramType string) string {
	device := "mobile phone"
	if diagramType == BoardDiagramUIDesktop {
		device = "desktop web"
	}
	return strings.Join([]string{
		fmt.Sprintf("You are a senior product designer creating a %s app UI mockup.", device),
		"Convert the user's request into a screen layout described as STRICT JSON only.",
		"Think about real UX: a clear hierarchy, a navigation bar, primary and secondary actions, and realistic labels.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- Schema: {\"title\": string, \"screens\": [{\"name\": string, \"blocks\": [{\"type\": string, \"label\": string, \"variant\": string}]}]}.",
		"- A block \"type\" is one of: navbar, heading, subheading, text, input, search, button, image, card, list, avatar, divider, link, tabbar.",
		"- \"label\" is the visible text for that component (keep it realistic and concise).",
		"- For button, \"variant\" is \"primary\" or \"secondary\". For other types leave \"variant\" empty.",
		"- Order blocks top-to-bottom as they should appear on screen. Put navbar first and tabbar (if any) last.",
		"- Design 1 to 3 screens. Use 6 to 16 blocks per screen.",
	}, "\n")
}

// parseUIMockup extracts and validates the model's JSON object.
func parseUIMockup(raw string) (*uiMockupRaw, error) {
	js := extractJSONObject(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var m uiMockupRaw
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		return nil, err
	}
	// Normalize a top-level blocks response into a single screen.
	if len(m.Screens) == 0 && len(m.Blocks) > 0 {
		m.Screens = []uiScreenRaw{{Name: m.Title, Blocks: m.Blocks}}
	}
	if len(m.Screens) == 0 {
		return nil, fmt.Errorf("no screens")
	}
	return &m, nil
}

// device geometry presets.
type uiGeom struct {
	frameW  float64
	padding float64
	gap     float64
	barH    float64
	minH    float64
	btnW    float64 // 0 = full content width
	imageH  float64
}

func uiGeomFor(diagramType string) uiGeom {
	if diagramType == BoardDiagramUIDesktop {
		return uiGeom{frameW: 1200, padding: 40, gap: 18, barH: 60, minH: 760, btnW: 200, imageH: 220}
	}
	return uiGeom{frameW: 390, padding: 18, gap: 14, barH: 56, minH: 780, btnW: 0, imageH: 170}
}

// Text-metric constants for server-side wrapping. They MUST stay in sync with
// the font sizes the client (boardAIPanel uiComponentSkeleton) renders each
// text variant at, so the height we reserve here matches what is drawn and the
// mockup never has text overflowing its box or overlapping the next block.
//   - uiCharWFactor is a deliberately conservative average glyph width (as a
//     fraction of font size) so a wrapped line is always a touch narrower than
//     its box, never wider.
//   - uiLineHFactor is the line height as a fraction of font size.
const (
	uiCharWFactor = 0.58
	uiLineHFactor = 1.3
)

// uiFontSize is the render font size for a text variant (must match the client).
func uiFontSize(variant string) float64 {
	switch variant {
	case "heading":
		return 28
	case "subheading", "title":
		return 20
	case "label":
		return 14
	case "tab":
		return 12
	default: // body, placeholder, link
		return 16
	}
}

// wrapUIText word-wraps a label to fit width at its variant's font size, capped
// to maxLines (overflow is truncated with an ellipsis). A single word longer
// than a line is hard-broken. Returns the newline-joined text and its line
// count, so the layout can reserve the exact height the client will draw.
func wrapUIText(label string, width float64, variant string, maxLines int) (string, int) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", 1
	}
	maxChars := int(width / (uiFontSize(variant) * uiCharWFactor))
	if maxChars < 4 {
		maxChars = 4
	}
	var lines []string
	cur := ""
	flush := func() {
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
	}
	for _, w := range strings.Fields(label) {
		for len(w) > maxChars {
			flush()
			lines = append(lines, w[:maxChars])
			w = w[maxChars:]
		}
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= maxChars:
			cur += " " + w
		default:
			flush()
			cur = w
		}
	}
	flush()
	if len(lines) == 0 {
		return "", 1
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		last := lines[maxLines-1]
		if len(last) > maxChars-1 {
			last = last[:maxChars-1]
		}
		lines[maxLines-1] = strings.TrimRight(last, " ") + "…"
	}
	return strings.Join(lines, "\n"), len(lines)
}

// uiTextH is the vertical space to reserve for a wrapped text block.
func uiTextH(lines int, variant string) float64 {
	if lines < 1 {
		lines = 1
	}
	return float64(lines) * (uiFontSize(variant) * uiLineHFactor)
}

// layoutUIMockup validates then lays each screen out into a device frame.
func layoutUIMockup(m *uiMockupRaw, diagramType string) *BoardGenerateResult {
	g := uiGeomFor(diagramType)
	device := "mobile"
	if diagramType == BoardDiagramUIDesktop {
		device = "desktop"
	}

	res := &BoardGenerateResult{
		Title:  sanitizeLabel(m.Title),
		Type:   diagramType,
		Device: device,
	}
	if res.Title == "" {
		res.Title = "Untitled mockup"
	}

	screenGap := 80.0
	screenX := originX
	screensLaid := 0

	for _, scr := range m.Screens {
		if screensLaid >= uiMaxScreens {
			break
		}
		comps, frameH := layoutUIScreen(scr, g, screenX, originY)
		if len(comps) == 0 {
			continue
		}
		name := sanitizeLabel(scr.Name)
		if name == "" {
			name = fmt.Sprintf("Screen %d", screensLaid+1)
		}
		res.Frames = append(res.Frames, BoardLaidFrame{
			Name: name, X: screenX, Y: originY, W: g.frameW, H: frameH,
		})
		res.Components = append(res.Components, comps...)
		screenX += g.frameW + screenGap
		screensLaid++
	}
	return res
}

// layoutUIScreen stacks one screen's blocks vertically inside a device frame
// anchored at (fx, fy). Returns the laid-out components and the final frame
// height. navbar/tabbar are full-bleed; everything else respects the padding.
func layoutUIScreen(scr uiScreenRaw, g uiGeom, fx, fy float64) ([]BoardLaidComponent, float64) {
	contentX := fx + g.padding
	contentW := g.frameW - 2*g.padding
	y := fy
	var comps []BoardLaidComponent

	add := func(c BoardLaidComponent) { comps = append(comps, c) }

	var tabbar *uiBlockRaw
	count := 0
	for i := range scr.Blocks {
		if count >= uiMaxBlocksPerScrn {
			break
		}
		b := scr.Blocks[i]
		typ := strings.ToLower(strings.TrimSpace(b.Type))
		label := sanitizeLabel(b.Label)

		switch typ {
		case "navbar", "appbar", "header", "topbar":
			add(BoardLaidComponent{Role: "navbar", X: fx, Y: y, W: g.frameW, H: g.barH})
			if label != "" {
				txt, _ := wrapUIText(label, contentW, "title", 1)
				add(BoardLaidComponent{Role: "text", Variant: "title", Text: txt, X: contentX, Y: y + (g.barH-22)/2, W: contentW, H: 22})
			}
			y += g.barH + g.gap
		case "tabbar", "bottomnav", "navigation":
			// Defer to the bottom of the frame.
			bb := b
			tabbar = &bb
		case "heading", "title", "h1":
			if label == "" {
				label = "Heading"
			}
			txt, lines := wrapUIText(label, contentW, "heading", 3)
			h := uiTextH(lines, "heading")
			add(BoardLaidComponent{Role: "text", Variant: "heading", Text: txt, X: contentX, Y: y, W: contentW, H: h})
			y += h + g.gap
		case "subheading", "h2":
			txt, lines := wrapUIText(label, contentW, "subheading", 2)
			h := uiTextH(lines, "subheading")
			add(BoardLaidComponent{Role: "text", Variant: "subheading", Text: txt, X: contentX, Y: y, W: contentW, H: h})
			y += h + g.gap
		case "text", "paragraph", "body", "label":
			txt, lines := wrapUIText(label, contentW, "body", 12)
			h := uiTextH(lines, "body")
			add(BoardLaidComponent{Role: "text", Variant: "body", Text: txt, X: contentX, Y: y, W: contentW, H: h})
			y += h + g.gap
		case "link":
			if label == "" {
				label = "Learn more"
			}
			txt, lines := wrapUIText(label, contentW, "link", 2)
			h := uiTextH(lines, "link")
			add(BoardLaidComponent{Role: "text", Variant: "link", Text: txt, X: contentX, Y: y, W: contentW, H: h})
			y += h + g.gap
		case "input", "field", "textfield", "textarea":
			if label != "" {
				lt, _ := wrapUIText(label, contentW, "label", 1)
				add(BoardLaidComponent{Role: "text", Variant: "label", Text: lt, X: contentX, Y: y, W: contentW, H: 16})
				y += 20
			}
			add(BoardLaidComponent{Role: "input", X: contentX, Y: y, W: contentW, H: 42})
			ph := label
			if ph == "" {
				ph = "Enter value"
			}
			pt, _ := wrapUIText(ph, contentW-24, "placeholder", 1)
			add(BoardLaidComponent{Role: "text", Variant: "placeholder", Text: pt, X: contentX + 12, Y: y + 12, W: contentW - 24, H: 18})
			y += 42 + g.gap
		case "search", "searchbar":
			add(BoardLaidComponent{Role: "input", X: contentX, Y: y, W: contentW, H: 42})
			ph := label
			if ph == "" {
				ph = "Search"
			}
			pt, _ := wrapUIText(ph, contentW-24, "placeholder", 1)
			add(BoardLaidComponent{Role: "text", Variant: "placeholder", Text: pt, X: contentX + 12, Y: y + 12, W: contentW - 24, H: 18})
			y += 42 + g.gap
		case "button", "cta":
			if label == "" {
				label = "Button"
			}
			variant := "primary"
			if strings.ToLower(strings.TrimSpace(b.Variant)) == "secondary" {
				variant = "secondary"
			}
			bw := contentW
			bx := contentX
			if g.btnW > 0 {
				bw = g.btnW
			}
			bt, _ := wrapUIText(label, bw-16, "label", 1)
			add(BoardLaidComponent{Role: "button", Variant: variant, Text: bt, X: bx, Y: y, W: bw, H: 44})
			y += 44 + g.gap
		case "image", "photo", "media", "hero":
			it, _ := wrapUIText(label, contentW-16, "label", 1)
			add(BoardLaidComponent{Role: "image", Text: it, X: contentX, Y: y, W: contentW, H: g.imageH})
			y += g.imageH + g.gap
		case "avatar":
			add(BoardLaidComponent{Role: "avatar", X: contentX, Y: y, W: 64, H: 64})
			y += 64 + g.gap
		case "card", "tile", "panel":
			if label != "" {
				txt, lines := wrapUIText(label, contentW-28, "label", 3)
				cardH := 96.0
				if lh := uiTextH(lines, "label") + 28; lh > cardH {
					cardH = lh
				}
				add(BoardLaidComponent{Role: "card", X: contentX, Y: y, W: contentW, H: cardH})
				add(BoardLaidComponent{Role: "text", Variant: "label", Text: txt, X: contentX + 14, Y: y + 14, W: contentW - 28, H: uiTextH(lines, "label")})
				y += cardH + g.gap
			} else {
				add(BoardLaidComponent{Role: "card", X: contentX, Y: y, W: contentW, H: 96})
				y += 96 + g.gap
			}
		case "list":
			rowH := 44.0
			rows := 3
			add(BoardLaidComponent{Role: "card", X: contentX, Y: y, W: contentW, H: rowH * float64(rows)})
			for r := 1; r < rows; r++ {
				ly := y + rowH*float64(r)
				add(BoardLaidComponent{Role: "divider", X: contentX, Y: ly, W: contentW, H: 1})
			}
			if label != "" {
				lt, _ := wrapUIText(label, contentW-28, "label", 1)
				add(BoardLaidComponent{Role: "text", Variant: "label", Text: lt, X: contentX + 14, Y: y + (rowH-18)/2, W: contentW - 28, H: 18})
			}
			y += rowH*float64(rows) + g.gap
		case "divider", "separator", "spacer":
			add(BoardLaidComponent{Role: "divider", X: contentX, Y: y, W: contentW, H: 1})
			y += g.gap
		default:
			// Unknown block: render a neutral card so nothing is silently lost.
			if label != "" {
				txt, lines := wrapUIText(label, contentW-28, "label", 2)
				cardH := 56.0
				if lh := uiTextH(lines, "label") + 28; lh > cardH {
					cardH = lh
				}
				add(BoardLaidComponent{Role: "card", X: contentX, Y: y, W: contentW, H: cardH})
				add(BoardLaidComponent{Role: "text", Variant: "label", Text: txt, X: contentX + 14, Y: y + 14, W: contentW - 28, H: uiTextH(lines, "label")})
				y += cardH + g.gap
			} else {
				add(BoardLaidComponent{Role: "card", X: contentX, Y: y, W: contentW, H: 56})
				y += 56 + g.gap
			}
		}
		count++
	}

	// Final frame height: content + bottom padding, at least the device min.
	frameH := y - fy + g.padding
	if frameH < g.minH {
		frameH = g.minH
	}

	// Bottom tab bar (mobile), pinned to the bottom of the frame.
	if tabbar != nil {
		tabY := fy + frameH - g.barH
		add(BoardLaidComponent{Role: "bar", X: fx, Y: tabY, W: g.frameW, H: g.barH})
		labels := splitTabLabels(tabbar.Label)
		if len(labels) > 0 {
			slot := g.frameW / float64(len(labels))
			for i, l := range labels {
				add(BoardLaidComponent{
					Role: "text", Variant: "tab", Text: l,
					X: fx + float64(i)*slot, Y: tabY + (g.barH-16)/2, W: slot, H: 16,
				})
			}
		}
	}

	return comps, frameH
}

// splitTabLabels turns "Home, Search, Profile" into individual tab labels,
// capped to a sensible number.
func splitTabLabels(s string) []string {
	s = sanitizeLabel(s)
	if s == "" {
		return []string{"Home", "Search", "Profile"}
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '|' || r == '/' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
		if len(out) >= 5 {
			break
		}
	}
	if len(out) == 0 {
		return []string{"Home", "Search", "Profile"}
	}
	return out
}
