// Package business is link previews (docs programme B4, PROGRAM.md §4 #20):
// the title, description and icon a doc's bookmark and link mention show, read
// by this server from the page itself, so a self-hosted box never sends its
// people's links to a preview service.
//
// A preview is read through helpers/safefetch, which refuses every address
// inside the network. Known video, music and design sites answer through their
// oEmbed endpoints; every other page through its Open Graph and Twitter card
// tags, its <title> and its icons. Cards are cached for a day, failures for ten
// minutes, so a link pasted into many docs is fetched once.
//
// A preview that can't be read is never an error to the person: the card says
// it wasn't fetched and why, and the doc shows a plain link.
package business

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// Card is a link's preview.
type Card struct {
	// URL is the link as it was asked for, without tracking parameters.
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	// IconURL and ImageURL are where the site's icon and picture are. The web
	// app never loads them from there: it asks this server to store the icon
	// with the doc (POST /unfurl/store).
	IconURL  string `json:"icon_url,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	// Kind is the page's type as it says (og:type, or oEmbed's type).
	Kind string `json:"kind,omitempty"`
	// Provider is the site whose oEmbed endpoint answered: youtube, vimeo,
	// loom, spotify, x, codepen, figma or miro.
	Provider string `json:"provider,omitempty"`
	Author   string `json:"author,omitempty"`
	// Fetched is whether the page was read. When it wasn't, Reason says why.
	Fetched bool   `json:"fetched"`
	Reason  string `json:"reason,omitempty"`
}

// Why a preview wasn't read.
const (
	// ReasonBlocked: the address is inside the network, or not a web page's.
	ReasonBlocked = "blocked"
	// ReasonUnreachable: the site couldn't be reached from this server.
	ReasonUnreachable = "unreachable"
	// ReasonTimeout: the site took too long.
	ReasonTimeout = "timeout"
	// ReasonNotHTML: the link is to a file, not a page.
	ReasonNotHTML = "not_html"
	// ReasonBadStatus: the site answered with an error.
	ReasonBadStatus = "bad_status"
	// ReasonSignIn: the page is behind a sign-in.
	ReasonSignIn = "sign_in"
)

// ErrInvalidURL is a link that isn't an http or https address.
var ErrInvalidURL = errors.New("unfurl: not an http or https link")

// maxURLLength bounds a link that is asked about.
const maxURLLength = 2048

// trackingParams are query parameters that say where a click came from, never
// what the page is. They are stripped from the link (and so from the cache's
// key): two people sharing one article share one card.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "gbraid": true, "wbraid": true, "msclkid": true,
	"mc_cid": true, "mc_eid": true, "igshid": true, "si": true, "ref_src": true, "ref_url": true,
	"_hsenc": true, "_hsmi": true, "mkt_tok": true, "yclid": true, "twclid": true, "ttclid": true,
	"vero_id": true, "oly_enc_id": true, "oly_anon_id": true, "rb_clickid": true, "s_cid": true,
}

// isTracking reports whether a query parameter is a tracking one. Pure.
func isTracking(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "utm_") || trackingParams[n]
}

// Canonical parses rawURL as an http or https link and strips its tracking
// parameters. The fragment stays (a link to a heading is that heading).
// Whether the link may be fetched is safefetch's to say, not this. Pure.
func Canonical(rawURL string) (*url.URL, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" || len(raw) > maxURLLength {
		return nil, ErrInvalidURL
	}
	// url.Parse lowercases the scheme, and an opaque address has no host.
	u, ok := helpers.WebLink(raw)
	if !ok {
		return nil, ErrInvalidURL
	}
	u.Host = strings.ToLower(u.Host)
	if u.RawQuery != "" {
		// Kept in its order, minus the trackers: the order can matter to a site.
		var kept []string
		for _, pair := range strings.Split(u.RawQuery, "&") {
			name := pair
			if i := strings.IndexByte(pair, '='); i >= 0 {
				name = pair[:i]
			}
			if decoded, err := url.QueryUnescape(name); err == nil {
				name = decoded
			}
			if pair != "" && !isTracking(name) {
				kept = append(kept, pair)
			}
		}
		u.RawQuery = strings.Join(kept, "&")
		u.ForceQuery = false
	}
	return u, nil
}

// cacheKey is the cache's key for a canonical link: its SHA-256, without the
// fragment (the page is the same page). Pure.
func cacheKey(u *url.URL) string {
	c := *u
	c.Fragment, c.RawFragment = "", ""
	sum := sha256.Sum256([]byte(c.String()))
	return hex.EncodeToString(sum[:])
}
