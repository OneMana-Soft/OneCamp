package business

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/akashc777/OneCamp/helpers"
)

// How long each of a card's words may be, in characters.
const (
	maxTitle       = 300
	maxDescription = 500
	maxSiteName    = 100
	maxAuthor      = 100
	// maxTokens bounds the work a page can make the parser do, whatever its
	// size: a page's head, where previews live, is far fewer.
	maxTokens = 20000
)

// pageMeta is what a page says about itself.
type pageMeta struct {
	title, ogTitle, twTitle         string
	desc, ogDesc, twDesc            string
	siteName, appName               string
	ogImage, ogImageSecure, twImage string
	ogType                          string
	icons                           []pageIcon
	oembed                          string
}

type pageIcon struct{ href, rel, sizes, typ string }

// readPage reads what a page says about itself from its HTML, in whatever
// character set it is written. It never runs a script and never follows a
// refresh. Pure.
func readPage(body []byte, contentType string) pageMeta {
	var m pageMeta
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		r = bytes.NewReader(body)
	}
	z := html.NewTokenizer(r)
	inTitle := false
	for i := 0; i < maxTokens; i++ {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.TextToken:
			if inTitle && m.title == "" {
				m.title = string(z.Text())
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				inTitle = false
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "title":
				inTitle = tt == html.StartTagToken
			case "meta":
				if hasAttr {
					m.meta(attrs(z))
				}
			case "link":
				if hasAttr {
					m.link(attrs(z))
				}
			}
		}
	}
	return m
}

func attrs(z *html.Tokenizer) map[string]string {
	out := map[string]string{}
	for {
		k, v, more := z.TagAttr()
		key := strings.ToLower(string(k))
		if _, seen := out[key]; !seen {
			out[key] = string(v)
		}
		if !more {
			return out
		}
	}
}

func (m *pageMeta) meta(a map[string]string) {
	key := strings.ToLower(firstOf(a["property"], a["name"], a["itemprop"]))
	content := a["content"]
	if content == "" {
		return
	}
	set := func(dst *string) {
		if *dst == "" {
			*dst = content
		}
	}
	switch key {
	case "og:title":
		set(&m.ogTitle)
	case "twitter:title":
		set(&m.twTitle)
	case "og:description":
		set(&m.ogDesc)
	case "twitter:description":
		set(&m.twDesc)
	case "description":
		set(&m.desc)
	case "og:site_name":
		set(&m.siteName)
	case "application-name":
		set(&m.appName)
	case "og:image", "og:image:url":
		set(&m.ogImage)
	case "og:image:secure_url":
		set(&m.ogImageSecure)
	case "twitter:image", "twitter:image:src":
		set(&m.twImage)
	case "og:type":
		set(&m.ogType)
	}
}

func (m *pageMeta) link(a map[string]string) {
	rel := strings.ToLower(strings.TrimSpace(a["rel"]))
	href := strings.TrimSpace(a["href"])
	if href == "" {
		return
	}
	typ := strings.ToLower(a["type"])
	if strings.Contains(rel, "alternate") && typ == "application/json+oembed" && m.oembed == "" {
		m.oembed = href
		return
	}
	for _, word := range strings.Fields(rel) {
		if word == "icon" || word == "apple-touch-icon" || word == "apple-touch-icon-precomposed" {
			m.icons = append(m.icons, pageIcon{href: href, rel: rel, sizes: strings.ToLower(a["sizes"]), typ: typ})
			return
		}
	}
}

// bestIcon picks the icon a card shows: a small raster one (16 to 64 px)
// before a large touch icon, never an SVG or a mask (the store refuses SVG),
// and the site's /favicon.ico when it names none. Pure.
func bestIcon(icons []pageIcon, base *url.URL) string {
	best, bestScore := "", -1
	for _, ic := range icons {
		if ic.typ == "image/svg+xml" || strings.Contains(ic.rel, "mask") {
			continue
		}
		abs := absURL(base, ic.href)
		if abs == "" || strings.HasSuffix(strings.ToLower(strings.SplitN(abs, "?", 2)[0]), ".svg") {
			continue
		}
		score := 1
		if strings.Contains(ic.rel, "apple-touch") {
			score = 2
		} else if size := sizeOf(ic.sizes); size >= 16 && size <= 64 {
			score = 4
		} else if size == 0 {
			score = 3
		}
		if score > bestScore {
			best, bestScore = abs, score
		}
	}
	if best != "" {
		return best
	}
	return absURL(base, "/favicon.ico")
}

// sizeOf reads "32x32" (the first of several) as 32, and anything else as 0.
func sizeOf(sizes string) int {
	first := strings.Fields(sizes)
	if len(first) == 0 {
		return 0
	}
	w, _, ok := strings.Cut(first[0], "x")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(w)
	if err != nil {
		return 0
	}
	return n
}

// absURL resolves a reference against the page's address, and answers it only
// when it is an http or https address. Pure.
func absURL(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > maxURLLength {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil {
		r = base.ResolveReference(r)
	}
	if !helpers.IsWebURL(r) {
		return ""
	}
	return r.String()
}

// fill puts what a page says into a card: Open Graph first, then Twitter's
// card, then the page's own title and description.
func (m pageMeta) fill(c *Card, base *url.URL) {
	c.Title = clean(firstOf(m.ogTitle, m.twTitle, m.title), maxTitle)
	c.Description = clean(firstOf(m.ogDesc, m.twDesc, m.desc), maxDescription)
	c.SiteName = clean(firstOf(m.siteName, m.appName), maxSiteName)
	c.ImageURL = absURL(base, firstOf(m.ogImageSecure, m.ogImage, m.twImage))
	c.IconURL = bestIcon(m.icons, base)
	c.Kind = clean(m.ogType, 40)
}

// oembed is the part of an oEmbed answer a card reads.
type oembed struct {
	Type         string `json:"type"`
	Title        string `json:"title"`
	AuthorName   string `json:"author_name"`
	AuthorURL    string `json:"author_url"`
	ProviderName string `json:"provider_name"`
	ThumbnailURL string `json:"thumbnail_url"`
	HTML         string `json:"html"`
}

// readOEmbed reads an oEmbed answer into a card. A post on X has no title: it
// is "<author> on X", and its words are the post's text. Pure.
func readOEmbed(body []byte, p provider, c *Card) bool {
	var o oembed
	if err := json.Unmarshal(body, &o); err != nil {
		return false
	}
	c.Kind = clean(o.Type, 40)
	c.Author = clean(o.AuthorName, maxAuthor)
	c.SiteName = clean(firstOf(o.ProviderName, p.site), maxSiteName)
	c.ImageURL = absURL(nil, o.ThumbnailURL)
	c.Title = clean(o.Title, maxTitle)
	if p.id == "x" {
		if c.Author != "" {
			c.Title = clean(c.Author+" on X", maxTitle)
		}
		c.Description = clean(firstParagraph(o.HTML), maxDescription)
	}
	return c.Title != "" || c.Description != ""
}

// firstParagraph is the text of the first <p> in an oEmbed answer's HTML,
// tags and all markup dropped. Pure.
func firstParagraph(fragment string) string {
	z := html.NewTokenizer(strings.NewReader(fragment))
	depth := 0
	var b strings.Builder
	for i := 0; i < maxTokens; i++ {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "p" {
				depth++
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "p" && depth > 0 {
				return b.String()
			}
		case html.TextToken:
			if depth > 0 {
				b.Write(z.Text())
			}
		}
	}
	return b.String()
}

// clean makes a page's words fit a card: entities already decoded, control
// characters out, whitespace collapsed, and at most max characters (cut with
// an ellipsis). Pure.
func clean(s string, max int) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}

func firstOf(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
