package business

// Embeds on a published page (docs C6-2, D-26): never a frame. The page is
// sandboxed on an opaque origin (D-25), and a frame inside it inherits that
// sandbox, so a player would neither run nor keep its own origin. An embed
// the workspace allows becomes a card instead, naming the site, with a link
// to it; any other stays a plain link.
//
// The sites are the web app's built-in embeds (lib/embeds/providers.ts), by
// host only: the page never makes a frame's address, so the card needs the
// site's name and nothing else. The admin's hosts (the policy
// embed.allowlist) are read as the web reads them (hostAllowed): https only.

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/akashc777/OneCamp/helpers"
)

// builtInSites are the hosts of the web app's built-in embeds, without
// "www.", and the site's name as people say it. TestTheCardsNameTheWebsSites
// holds it to lib/embeds/providers.ts's byHost.
var builtInSites = map[string]string{
	"youtube.com": "YouTube", "m.youtube.com": "YouTube", "music.youtube.com": "YouTube", "youtu.be": "YouTube", "youtube-nocookie.com": "YouTube",
	"vimeo.com": "Vimeo", "player.vimeo.com": "Vimeo",
	"loom.com":        "Loom",
	"figma.com":       "Figma",
	"docs.google.com": "Google Docs", "google.com": "Google Maps", "maps.google.com": "Google Maps",
	"gist.github.com":  "GitHub Gist",
	"codepen.io":       "CodePen",
	"excalidraw.com":   "Excalidraw",
	"miro.com":         "Miro",
	"open.spotify.com": "Spotify",
	"x.com":            "X", "twitter.com": "X", "mobile.twitter.com": "X",
}

// googleKinds name a docs.google.com link by what it is.
var googleKinds = map[string]string{
	"document": "Google Docs", "spreadsheets": "Google Sheets", "presentation": "Google Slides", "forms": "Google Forms",
}

// hostAllowed reports whether an admin's entry covers a host:
// "embed.example.com", or "*.example.com" for its subdomains. Pure; as
// lib/embeds/providers.ts hostAllowed.
func hostAllowed(host string, entries []string) bool {
	h := strings.ToLower(host)
	for _, raw := range entries {
		e := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, "*.") {
			if strings.HasSuffix(h, e[1:]) && len(h) > len(e)-1 {
				return true
			}
			continue
		}
		if h == e {
			return true
		}
	}
	return false
}

// embedSite is the site an embed's link is on, and its host as shown, when
// the workspace allows it; ok is false for any other link. Pure but for the
// workspace's own addresses (intoApp).
func embedSite(href string, adminHosts []string) (name, host string, ok bool) {
	u, ok := helpers.WebLink(strings.TrimSpace(href))
	if !ok || u.User != nil || intoApp(href) {
		return "", "", false
	}
	host = strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), "www.")
	if name, ok := builtInSites[host]; ok {
		if host == "docs.google.com" {
			if kind := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)[0]; googleKinds[kind] != "" {
				name = googleKinds[kind]
			}
		}
		if host == "google.com" && !strings.HasPrefix(u.Path, "/maps") {
			return "", "", false
		}
		return name, host, true
	}
	if u.Scheme == "https" && hostAllowed(strings.TrimSuffix(u.Hostname(), "."), adminHosts) {
		return host, host, true
	}
	return "", "", false
}

// embedCard is the card an allowed embed becomes: the site's name, the
// link's host, and "Open on <site>". A plain link: the page's sandbox opens
// no new windows, and the page sends no referrer.
func embedCard(href, name, host string) *html.Node {
	card := element(atom.Div, html.Attribute{Key: "class", Val: "embed"})
	site := element(atom.Span, html.Attribute{Key: "class", Val: "es"})
	site.AppendChild(text(name))
	where := element(atom.Span, html.Attribute{Key: "class", Val: "eh"})
	where.AppendChild(text(host))
	open := element(atom.A, html.Attribute{Key: "href", Val: href})
	open.AppendChild(text("Open on " + name))
	card.AppendChild(site)
	card.AppendChild(where)
	card.AppendChild(open)
	return card
}
