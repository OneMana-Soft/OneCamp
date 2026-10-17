package contentcodec

// The codec's rules for what a link or a file becomes in a doc (docs programme
// B4): a bookmark, an embed, a file block (a video, audio, a PDF or any file)
// and a link mention, plus an image's alignment, crop and link. The same rules,
// in the same order, as the web's lib/codec/media.ts and mediaLink.ts; the
// golden fixture testdata/codec/media.* holds the two to the same bytes.
//
// In HTML each block is the div the editor renders, its attributes as data-*,
// holding a plain link to what it points at, so a reader that knows nothing of
// it (another editor, a search index, an email) still has the link and its
// words. In Markdown each is that link.

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

type mediaBlock struct {
	dataType string
	attrs    [][2]string // [node attribute, HTML attribute], in the order written
}

var mediaBlocks = map[string]mediaBlock{
	"bookmark": {"bookmark", [][2]string{
		{"href", "data-href"}, {"title", "data-title"}, {"description", "data-description"},
		{"siteName", "data-site"}, {"icon", "data-icon"}, {"image", "data-image"},
	}},
	"webEmbed": {"web-embed", [][2]string{{"src", "data-src"}, {"height", "data-height"}}},
	"fileBlock": {"file", [][2]string{
		{"kind", "data-kind"}, {"src", "data-src"}, {"id", "data-id"}, {"name", "data-name"},
		{"size", "data-size"}, {"mime", "data-mime"}, {"caption", "data-caption"},
	}},
}

var mentionAttrs = [][2]string{{"title", "data-title"}, {"icon", "data-icon"}, {"siteName", "data-site"}}

// imageMediaAttrs are an image's own attributes beyond Tiptap's, written as
// data-* after its others.
var imageMediaAttrs = [][2]string{{"align", "data-align"}, {"crop", "data-crop"}, {"href", "data-href"}}

var fileKinds = map[string]bool{"file": true, "video": true, "audio": true, "pdf": true}

var imageAligns = map[string]bool{"left": true, "center": true, "full": true}

// cropPattern is four fractions of the picture: left, top, width, height.
var cropPattern = regexp.MustCompile(`^(0|1|0\.\d{1,4})(,(0|1|0\.\d{1,4})){3}$`)

func init() {
	none := func(names ...string) []attrSpec {
		out := make([]attrSpec, 0, len(names))
		for _, n := range names {
			out = append(out, attrSpec{n, nil})
		}
		return out
	}
	nodeSpecs["bookmark"] = &nodeSpec{name: "bookmark", attrs: none("href", "title", "description", "siteName", "icon", "image")}
	nodeSpecs["webEmbed"] = &nodeSpec{name: "webEmbed", attrs: none("src", "height")}
	nodeSpecs["fileBlock"] = &nodeSpec{name: "fileBlock", attrs: append([]attrSpec{{"kind", "file"}}, none("src", "id", "name", "size", "mime", "caption")...)}
	nodeSpecs["linkMention"] = &nodeSpec{name: "linkMention", inline: true, attrs: none("href", "title", "icon", "siteName")}
	nodeSpecs["image"].attrs = append(nodeSpecs["image"].attrs, none("align", "crop", "href")...)
}

// jsSpace is JavaScript's \s, which the web's checks use.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func endsInSpace(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && jsSpace(r)
}

// webHref reports whether s is an absolute web address that is safe to
// follow, as written: no space around it.
func webHref(s string) bool {
	rest := ""
	switch {
	case hasPrefixFold(s, "http://"):
		rest = s[len("http://"):]
	case hasPrefixFold(s, "https://"):
		rest = s[len("https://"):]
	default:
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest != "" && !jsSpace(r) && !endsInSpace(s) && safeHref(s)
}

// fileHref reports whether s is a file's address: a web address, or one of
// the app's own paths.
func fileHref(s string) bool {
	if webHref(s) {
		return true
	}
	if len(s) < 2 || s[0] != '/' {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[1:])
	return r != '/' && r != '\\' && !jsSpace(r) && !endsInSpace(s) && safeHref(s)
}

// mediaLinkOf is the address and the words of the link a node stands for.
func mediaLinkOf(typ string, attrs map[string]any) (href, text string, ok bool) {
	file := typ == "fileBlock"
	key := "href"
	if file || typ == "webEmbed" {
		key = "src"
	}
	href = stringOf(attrs[key])
	if href == "" {
		return "", "", false
	}
	if file && !fileHref(href) || !file && !webHref(href) {
		return "", "", false
	}
	switch {
	case file:
		text = stringOf(attrs["name"])
	case typ != "webEmbed":
		text = stringOf(attrs["title"])
	}
	if text == "" {
		text = href
	}
	return href, text, true
}

// mediaLink is the link a block or a mention stands for, as a linked text
// node, or nil.
func mediaLink(n *Node) *Node {
	if _, block := mediaBlocks[n.Type]; !block && n.Type != "linkMention" {
		return nil
	}
	href, text, ok := mediaLinkOf(n.Type, n.Attrs)
	if !ok {
		return nil
	}
	marks := append(append([]*Mark{}, n.Marks...), &Mark{Type: "link", Attrs: map[string]any{"href": href}})
	return &Node{Type: "text", Text: text, Marks: marks}
}

// writeMediaBlock writes a block's HTML; false when n isn't one of these.
func writeMediaBlock(b *strings.Builder, n *Node) bool {
	mb, ok := mediaBlocks[n.Type]
	if !ok {
		return false
	}
	b.WriteString(`<div data-type="` + mb.dataType + `"`)
	for _, a := range mb.attrs {
		if v := n.attrString(a[0]); v != "" {
			writeAttr(b, a[1], v)
		}
	}
	b.WriteString(">")
	if href, text, ok := mediaLinkOf(n.Type, n.Attrs); ok {
		b.WriteString("<a")
		writeAttr(b, "href", href)
		b.WriteString(">" + escapeText(text) + "</a>")
	}
	b.WriteString("</div>")
	return true
}

// writeLinkMention writes a link mention: the link itself, with what the chip
// shows as data-*.
func writeLinkMention(b *strings.Builder, n *Node) {
	href, text, ok := mediaLinkOf(n.Type, n.Attrs)
	b.WriteString(`<a data-type="link-mention"`)
	if ok {
		writeAttr(b, "href", href)
	} else {
		text = n.attrString("title")
	}
	for _, a := range mentionAttrs {
		if v := n.attrString(a[0]); v != "" {
			writeAttr(b, a[1], v)
		}
	}
	b.WriteString(">" + escapeText(text) + "</a>")
}

// readMediaBlock reads a block from its div, or nil when it can't be one (no
// safe address), and the div is read as any other. Foreign HTML keeps no
// picture: an icon or an image there is another site's address, never one
// kept with the doc.
func readMediaBlock(dataType string, el *html.Node, foreign bool) *Node {
	typ := ""
	for k, mb := range mediaBlocks {
		if mb.dataType == dataType {
			typ = k
		}
	}
	if typ == "" {
		return nil
	}
	n := newNode(typ)
	n.Attrs = map[string]any{}
	for _, a := range mediaBlocks[typ].attrs {
		if v := attrVal(el, a[1]); v != "" {
			n.setAttr(a[0], v)
		}
	}
	if typ == "fileBlock" {
		if s := n.attrString("src"); s != "" && !fileHref(s) {
			delete(n.Attrs, "src")
		}
		if foreign && n.attrString("src") == "" {
			return nil
		}
	} else {
		key := "src"
		if typ == "bookmark" {
			key = "href"
		}
		if !webHref(n.attrString(key)) {
			return nil
		}
	}
	for _, a := range []string{"icon", "image"} {
		if _, has := n.Attrs[a]; has && (foreign || !fileHref(n.attrString(a))) {
			delete(n.Attrs, a)
		}
	}
	return n
}

// readLinkMention reads a link mention, or nil to read the element as a link.
func readLinkMention(el *html.Node, foreign bool) *Node {
	href := attrVal(el, "href")
	if !webHref(href) {
		return nil
	}
	n := newNode("linkMention")
	n.setAttr("href", href)
	for _, a := range mentionAttrs {
		if v := attrVal(el, a[1]); v != "" && !(a[0] == "icon" && (foreign || !fileHref(v))) {
			n.setAttr(a[0], v)
		}
	}
	return n
}

// readImageMedia reads an image's alignment, crop and link from its element,
// when they are sound.
func readImageMedia(el *html.Node, img *Node) {
	if v := attrVal(el, "data-align"); v != "" && imageAligns[v] {
		img.setAttr("align", v)
	}
	if v := attrVal(el, "data-crop"); v != "" && cropPattern.MatchString(v) {
		img.setAttr("crop", v)
	}
	if v := attrVal(el, "data-href"); v != "" && webHref(v) {
		img.setAttr("href", v)
	}
}

// typedMediaAttr is an attribute of these blocks as the editor types it; ok
// is false when it isn't one of theirs.
func typedMediaAttr(typ, name string, v any) (any, bool) {
	switch {
	case typ == "webEmbed" && name == "height", typ == "fileBlock" && name == "size":
		i := -1.0
		switch t := v.(type) {
		case float64:
			i = math.Trunc(t)
		case int:
			i = float64(t)
		case string:
			if t != "" && strings.Trim(t, "0123456789") == "" {
				if f, err := strconv.ParseFloat(t, 64); err == nil {
					i = f
				}
			}
		}
		if i < 0 {
			return nil, true
		}
		if typ == "webEmbed" && i > 4000 {
			i = 4000
		}
		return i, true
	case typ == "fileBlock" && name == "kind":
		if s, ok := v.(string); ok && fileKinds[s] {
			return s, true
		}
		return "file", true
	}
	return nil, false
}
