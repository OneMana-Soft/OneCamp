package contentcodec

// The technical blocks' attributes (docs programme B3): a table cell's
// column widths and colour, a code block's wrap and caption, and an
// equation's TeX. typedAttr asks here first, so these cases live with B3
// rather than in the shared switch.
func typedTechnicalAttr(typ, name string, v any) (any, bool) {
	cell := typ == "tableCell" || typ == "tableHeader"
	switch {
	case cell && name == "colwidth":
		// A column's width in px, one for each column the cell spans.
		ws, ok := v.([]any)
		if !ok || len(ws) == 0 {
			return nil, true
		}
		for _, w := range ws {
			f, ok := w.(float64)
			if !ok || f <= 0 || f != float64(int64(f)) {
				return nil, true
			}
		}
		return ws, true
	case cell && name == "background":
		if s, ok := v.(string); ok && IsDocColour(s) {
			return s, true
		}
		return nil, true
	case typ == "codeBlock" && name == "wrap":
		if v == true || v == "true" {
			return true, true
		}
		return nil, true
	case typ == "codeBlock" && name == "caption", name == "latex":
		if s, ok := v.(string); ok {
			return s, true
		}
		return nil, true
	}
	return nil, false
}
