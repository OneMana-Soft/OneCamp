package contentcodec

// What a node or a mark may point at (V-528 (1), CR-220 F1): ONE admission
// per type, which every reader of a body applies to the attributes it read,
// whether from the element the editor renders (html_in.go, media.go) or from a
// marker the collaboration service kept (preserved.go). The two paths read the
// same attributes from different places; they never check them apart, so a
// marker can't carry an address its tag twin would lose.

// admitMark checks a mark's attributes in place: an attribute it may not keep
// is dropped, and false says the mark can't stand (a link with no safe
// address). Foreign HTML keeps a link's address only.
func admitMark(m *Mark, foreign bool) bool {
	if m.Type != "link" {
		return true
	}
	if !keepAddress(m.Attrs, "href", safeHref) {
		return false
	}
	if foreign {
		for _, a := range []string{"target", "rel", "class"} {
			delete(m.Attrs, a)
		}
	}
	if _, has := m.Attrs["class"]; has {
		if c := contentClasses(stringOf(m.Attrs["class"])); c != "" {
			m.Attrs["class"] = c
		} else {
			delete(m.Attrs, "class")
		}
	}
	return true
}

// admitNode checks a node's attributes in place, as admitMark does a mark's:
// false when the node can't stand, and then refusedText is what it reads as.
// Foreign HTML keeps no picture of its own (an icon or an image there is
// another site's address, never one kept with the doc), and a file block's
// relative path counts only in an import that brings its files (assets).
func admitNode(n *Node, foreign, assets bool) bool {
	a := n.Attrs // reading or deleting from a nil map is safe
	switch n.Type {
	case "image":
		if s, ok := a["src"].(string); ok {
			a["src"] = trimSpace(s)
		}
		if !keepAddress(a, "src", func(s string) bool { return safeImageSrc(s, true) }) {
			return false
		}
		if foreign {
			for _, k := range []string{"id", "caption", "fileName", "align", "crop", "href"} {
				delete(a, k)
			}
		}
		keepAddress(a, "align", func(s string) bool { return imageAligns[s] })
		keepAddress(a, "crop", cropPattern.MatchString)
		keepAddress(a, "href", webHref)
	case "bookmark", "webEmbed", "linkMention":
		key := "href"
		if n.Type == "webEmbed" {
			key = "src"
		}
		if !keepAddress(a, key, webHref) {
			return false
		}
		for _, k := range []string{"icon", "image"} {
			keepAddress(a, k, func(s string) bool { return !foreign && fileHref(s) })
		}
	case "fileBlock":
		keepAddress(a, "src", func(s string) bool { return fileHref(s) || assets && importPath(s) })
		if foreign && n.attrString("src") == "" {
			return false
		}
	}
	return true
}

// refusedText is what a node admitNode refused reads as: the words its
// element shows (an image's alt text, a link mention's title).
func refusedText(n *Node) string {
	switch n.Type {
	case "image":
		return trimSpace(n.attrString("alt"))
	case "linkMention":
		return n.attrString("title")
	}
	return ""
}

// keepAddress keeps attrs[key] when it is text that ok admits, and reports
// whether it is kept; anything else there is dropped.
func keepAddress(attrs map[string]any, key string, ok func(string) bool) bool {
	v, has := attrs[key]
	if !has {
		return false
	}
	if s, isText := v.(string); isText && ok(s) {
		return true
	}
	delete(attrs, key)
	return false
}
