package miro

// How each of Miro's named values comes over: shapes, connector caps, fonts
// and tag colours. Every value Miro's spec names (21 Oct 2026) is listed, so a
// value it adds later is the only kind that falls to a default, and the
// report says the board had one.

import "github.com/akashc777/OneCamp/business/Board/scene"

// shapeKind is how a Miro shape is drawn: as one of Excalidraw's own shapes
// (rectangle, ellipse, diamond; round for a rounded rectangle), or as a closed
// line through outline points (each 0..1 of the box), or an open one.
type shapeKind struct {
	kind    string // "rectangle", "ellipse", "diamond" or "line"
	round   bool
	outline []scene.Point
	open    bool // a line whose ends don't meet (a brace)
	near    bool // drawn as the nearest shape there is: the report says so
}

func pts(xy ...float64) []scene.Point {
	out := make([]scene.Point, 0, len(xy)/2)
	for i := 0; i+1 < len(xy); i += 2 {
		out = append(out, scene.Point{X: xy[i], Y: xy[i+1]})
	}
	return out
}

// closed is an outline whose last point is its first, so Excalidraw fills it.
func closed(p []scene.Point) []scene.Point { return append(p, p[0]) }

// shapes are Miro's shapes (ShapeData.shape, REST v2) and the experimental
// flowchart set's commonest.
var shapes = map[string]shapeKind{
	"rectangle":                     {kind: "rectangle"},
	"round_rectangle":               {kind: "rectangle", round: true},
	"circle":                        {kind: "ellipse"},
	"rhombus":                       {kind: "diamond"},
	"triangle":                      {kind: "line", outline: closed(pts(0.5, 0, 1, 1, 0, 1))},
	"parallelogram":                 {kind: "line", outline: closed(pts(0.25, 0, 1, 0, 0.75, 1, 0, 1))},
	"trapezoid":                     {kind: "line", outline: closed(pts(0.25, 0, 0.75, 0, 1, 1, 0, 1))},
	"pentagon":                      {kind: "line", outline: closed(pts(0.5, 0, 1, 0.38, 0.81, 1, 0.19, 1, 0, 0.38))},
	"hexagon":                       {kind: "line", outline: closed(pts(0.25, 0, 0.75, 0, 1, 0.5, 0.75, 1, 0.25, 1, 0, 0.5))},
	"octagon":                       {kind: "line", outline: closed(pts(0.29, 0, 0.71, 0, 1, 0.29, 1, 0.71, 0.71, 1, 0.29, 1, 0, 0.71, 0, 0.29))},
	"star":                          {kind: "line", outline: closed(pts(0.5, 0, 0.62, 0.35, 1, 0.38, 0.7, 0.6, 0.81, 1, 0.5, 0.78, 0.19, 1, 0.3, 0.6, 0, 0.38, 0.38, 0.35))},
	"cross":                         {kind: "line", outline: closed(pts(0.35, 0, 0.65, 0, 0.65, 0.35, 1, 0.35, 1, 0.65, 0.65, 0.65, 0.65, 1, 0.35, 1, 0.35, 0.65, 0, 0.65, 0, 0.35, 0.35, 0.35))},
	"right_arrow":                   {kind: "line", outline: closed(pts(0, 0.25, 0.6, 0.25, 0.6, 0, 1, 0.5, 0.6, 1, 0.6, 0.75, 0, 0.75))},
	"left_arrow":                    {kind: "line", outline: closed(pts(1, 0.25, 0.4, 0.25, 0.4, 0, 0, 0.5, 0.4, 1, 0.4, 0.75, 1, 0.75))},
	"left_right_arrow":              {kind: "line", outline: closed(pts(0, 0.5, 0.25, 0, 0.25, 0.25, 0.75, 0.25, 0.75, 0, 1, 0.5, 0.75, 1, 0.75, 0.75, 0.25, 0.75, 0.25, 1))},
	"left_brace":                    {kind: "line", open: true, outline: pts(1, 0, 0.5, 0.05, 0.5, 0.45, 0, 0.5, 0.5, 0.55, 0.5, 0.95, 1, 1)},
	"right_brace":                   {kind: "line", open: true, outline: pts(0, 0, 0.5, 0.05, 0.5, 0.45, 1, 0.5, 0.5, 0.55, 0.5, 0.95, 0, 1)},
	"wedge_round_rectangle_callout": {kind: "line", outline: closed(pts(0, 0, 1, 0, 1, 0.75, 0.45, 0.75, 0.25, 1, 0.25, 0.75, 0, 0.75))},
	"flow_chart_predefined_process": {kind: "rectangle"},
	"cloud":                         {kind: "ellipse", near: true},
	"can":                           {kind: "rectangle", round: true, near: true},
	// The experimental flowchart set (v2-experimental), as its nearest.
	"flow_chart_process":            {kind: "rectangle"},
	"flow_chart_decision":           {kind: "diamond"},
	"flow_chart_terminator":         {kind: "rectangle", round: true},
	"flow_chart_connector":          {kind: "ellipse"},
	"flow_chart_input_output":       {kind: "line", outline: closed(pts(0.2, 0, 1, 0, 0.8, 1, 0, 1))},
	"flow_chart_manual_input":       {kind: "line", outline: closed(pts(0, 0.25, 1, 0, 1, 1, 0, 1))},
	"flow_chart_preparation":        {kind: "line", outline: closed(pts(0.2, 0, 0.8, 0, 1, 0.5, 0.8, 1, 0.2, 1, 0, 0.5))},
	"flow_chart_document":           {kind: "rectangle", near: true},
	"flow_chart_multidocuments":     {kind: "rectangle", near: true},
	"flow_chart_magnetic_disk":      {kind: "rectangle", round: true, near: true},
	"flow_chart_delay":              {kind: "rectangle", round: true, near: true},
	"flow_chart_merge":              {kind: "line", outline: closed(pts(0, 0, 1, 0, 0.5, 1))},
	"flow_chart_extract":            {kind: "line", outline: closed(pts(0.5, 0, 1, 1, 0, 1))},
	"flow_chart_off_page_connector": {kind: "line", outline: closed(pts(0, 0, 1, 0, 1, 0.75, 0.5, 1, 0, 0.75))},
}

// caps are Miro's connector caps (ConnectorStyle) as Excalidraw's arrowheads
// ("" for none), and whether the cap is only the nearest.
var caps = map[string]struct {
	head string
	near bool
}{
	"none":             {"", false},
	"stealth":          {"arrow", false},
	"rounded_stealth":  {"arrow", false},
	"arrow":            {"arrow", false},
	"triangle":         {"triangle_outline", false},
	"filled_triangle":  {"triangle", false},
	"diamond":          {"diamond_outline", false},
	"filled_diamond":   {"diamond", false},
	"oval":             {"circle_outline", false},
	"filled_oval":      {"circle", false},
	"erd_one":          {"crowfoot_one", false},
	"erd_many":         {"crowfoot_many", false},
	"erd_one_or_many":  {"crowfoot_one_or_many", false},
	"erd_only_one":     {"crowfoot_one", true},
	"erd_zero_or_one":  {"crowfoot_one", true},
	"erd_zero_or_many": {"crowfoot_many", true},
	"unknown":          {"arrow", true},
}

// fonts are Miro's thirty fonts (ShapeStyle.fontFamily) as Excalidraw's:
// mono faces as its code face, script faces as its hand-drawn one, display
// faces as its display face, the rest as the board's clean sans. Only the
// sans faces are exact; the report says when others were met.
var fonts = map[string]int{
	"arial": scene.FontSans, "open_sans": scene.FontSans, "pt_sans": scene.FontSans, "pt_sans_narrow": scene.FontSans,
	"roboto": scene.FontSans, "roboto_condensed": scene.FontSans, "noto_sans": scene.FontSans, "plex_sans": scene.FontSans,
	"formular": scene.FontSans, "graduate": scene.FontSans, "nixie_one": scene.FontSans,
	"eb_garamond": scene.FontSans, "georgia": scene.FontSans, "pt_serif": scene.FontSans, "roboto_slab": scene.FontSans,
	"times_new_roman": scene.FontSans, "plex_serif": scene.FontSans, "tiempos_text": scene.FontSans,
	"roboto_mono": scene.FontCode, "plex_mono": scene.FontCode,
	"caveat": scene.FontHand, "permanent_marker": scene.FontHand, "lemon_tuesday": scene.FontHand, "spoof": scene.FontHand,
	"bangers": scene.FontDisplay, "titan_one": scene.FontDisplay, "rammetto_one": scene.FontDisplay,
	"gravitas_one": scene.FontDisplay, "abril_fatface": scene.FontDisplay, "fredoka_one": scene.FontDisplay,
}

// sansExact are the fonts the clean sans stands in for without changing the
// look much; any other face is reported as approximated.
var sansExact = map[string]bool{
	"arial": true, "open_sans": true, "pt_sans": true, "pt_sans_narrow": true, "roboto": true,
	"roboto_condensed": true, "noto_sans": true, "plex_sans": true, "formular": true,
}
