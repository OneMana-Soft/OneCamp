package business

import (
	"net/url"
	"regexp"
	"strings"
)

// provider is a site whose links are previewed through its own oEmbed
// endpoint: a video, a song, a design or a post reads better from there than
// from a page that needs scripts to say anything.
type provider struct {
	id    string
	hosts []string
	// path is what a link's path must look like (a video, not the home page).
	path *regexp.Regexp
	// endpoint is the oEmbed address, with %s for the escaped link.
	endpoint string
	site     string
	icon     string
}

var providers = []provider{
	{id: "youtube", hosts: []string{"youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com"},
		path:     regexp.MustCompile(`^/(watch|shorts/|live/|embed/|playlist)`),
		endpoint: "https://www.youtube.com/oembed?format=json&url=%s", site: "YouTube", icon: "https://www.youtube.com/favicon.ico"},
	{id: "youtube", hosts: []string{"youtu.be"},
		path:     regexp.MustCompile(`^/[A-Za-z0-9_-]{6,}`),
		endpoint: "https://www.youtube.com/oembed?format=json&url=%s", site: "YouTube", icon: "https://www.youtube.com/favicon.ico"},
	{id: "vimeo", hosts: []string{"vimeo.com", "www.vimeo.com", "player.vimeo.com"},
		path:     regexp.MustCompile(`^/((video|channels/[^/]+|groups/[^/]+/videos)/)?\d+`),
		endpoint: "https://vimeo.com/api/oembed.json?url=%s", site: "Vimeo", icon: "https://vimeo.com/favicon.ico"},
	{id: "loom", hosts: []string{"loom.com", "www.loom.com"},
		path:     regexp.MustCompile(`^/(share|embed)/[0-9a-zA-Z]+`),
		endpoint: "https://www.loom.com/v1/oembed?url=%s", site: "Loom", icon: "https://www.loom.com/favicon.ico"},
	{id: "spotify", hosts: []string{"open.spotify.com"},
		path:     regexp.MustCompile(`^/(intl-[a-z-]+/)?(track|album|playlist|episode|show|artist)/`),
		endpoint: "https://open.spotify.com/oembed?url=%s", site: "Spotify", icon: "https://open.spotify.com/favicon.ico"},
	{id: "x", hosts: []string{"x.com", "www.x.com", "twitter.com", "www.twitter.com", "mobile.twitter.com"},
		path:     regexp.MustCompile(`^/[A-Za-z0-9_]{1,15}/status/\d+`),
		endpoint: "https://publish.twitter.com/oembed?omit_script=1&dnt=true&url=%s", site: "X", icon: "https://x.com/favicon.ico"},
	{id: "codepen", hosts: []string{"codepen.io"},
		path:     regexp.MustCompile(`^/[^/]+/(pen|full|details)/[A-Za-z0-9]+`),
		endpoint: "https://codepen.io/api/oembed?format=json&url=%s", site: "CodePen", icon: "https://codepen.io/favicon.ico"},
	{id: "figma", hosts: []string{"figma.com", "www.figma.com"},
		path:     regexp.MustCompile(`^/(file|design|proto|board|slides)/[A-Za-z0-9]+`),
		endpoint: "https://www.figma.com/api/oembed?url=%s", site: "Figma", icon: "https://www.figma.com/favicon.ico"},
	{id: "miro", hosts: []string{"miro.com"},
		path:     regexp.MustCompile(`^/app/board/[^/]+`),
		endpoint: "https://miro.com/api/v1/oembed?url=%s", site: "Miro", icon: "https://miro.com/favicon.ico"},
}

// providerFor answers the provider a link belongs to, if any. Pure.
func providerFor(u *url.URL) (provider, bool) {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	for _, p := range providers {
		for _, h := range p.hosts {
			if host == h && p.path.MatchString(u.EscapedPath()) {
				return p, true
			}
		}
	}
	return provider{}, false
}

// oembedURL is the provider's oEmbed address for a link. Pure.
func (p provider) oembedURL(link string) string {
	return strings.Replace(p.endpoint, "%s", url.QueryEscape(link), 1)
}
