package business

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

const (
	// unfurlTimeout bounds a whole preview: the page, and an oEmbed answer
	// when there is one. Each fetch keeps safefetch's own 5 s cap.
	unfurlTimeout = 8 * time.Second
	// failureTTL is how long a preview that couldn't be read is remembered,
	// so a site that is down isn't asked again on every render.
	failureTTL = 10 * time.Minute
)

// signInHosts are where a private page sends a visitor who isn't signed in:
// a preview of one would be the sign-in page's.
var signInHosts = map[string]bool{
	"accounts.google.com":       true,
	"login.microsoftonline.com": true,
	"login.live.com":            true,
}

// Seams for tests: the fetch, and the cache.
var (
	fetch    = safefetch.Fetch
	getCache = func(ctx context.Context, key string, c *Card) bool {
		ok, err := redisStore.GetJSON(ctx, registry.UnfurlCard, []string{key}, c)
		return ok && err == nil
	}
	setCache = func(ctx context.Context, key string, c Card, ttl time.Duration) {
		_ = redisStore.SetJSONWithTTL(ctx, registry.UnfurlCard, []string{key}, c, ttl)
	}
)

// Unfurl answers rawURL's preview card, from the cache when it has one.
// budget is asked before anything is fetched (only on a cache miss) and may
// refuse; its error is answered as is, and nothing is cached. ErrInvalidURL is
// a link that isn't http or https. Anything else that goes wrong is a card
// that says it wasn't fetched, and why.
func Unfurl(ctx context.Context, rawURL string, budget func() error) (Card, error) {
	u, err := Canonical(rawURL)
	if err != nil {
		return Card{}, err
	}
	key := cacheKey(u)
	var cached Card
	if getCache(ctx, key, &cached) {
		// The fragment isn't in the key: the card answers the link asked for.
		cached.URL = u.String()
		return cached, nil
	}
	if budget != nil {
		if err := budget(); err != nil {
			return Card{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, unfurlTimeout)
	defer cancel()
	card := read(ctx, u)
	ttl := registry.UnfurlCard.TTL
	if !card.Fetched {
		ttl = failureTTL
	}
	// Not under the request's context: a client that went away mustn't
	// waste the fetch it paid for.
	setCache(context.WithoutCancel(ctx), key, card, ttl)
	return card, nil
}

// read fetches a link's preview: its provider's oEmbed answer when it has
// one, else the page.
func read(ctx context.Context, u *url.URL) Card {
	card := Card{URL: u.String()}
	if p, ok := providerFor(u); ok {
		if res, err := fetch(ctx, p.oembedURL(u.String()), safefetch.Options{Accept: "application/json"}); err == nil && res.Status == http.StatusOK && !res.Truncated {
			if readOEmbed(res.Body, p, &card) {
				card.Provider, card.Fetched = p.id, true
				card.IconURL = p.icon
				return card
			}
		}
		// A private video or a provider that's down: the page may still say
		// something.
		card = Card{URL: u.String()}
	}

	res, err := fetch(ctx, u.String(), safefetch.Options{Accept: "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5"})
	if err != nil {
		card.Reason = reasonFor(ctx, err)
		helpers.LogInfoWithContext(ctx, "business/Unfurl: %s wasn't previewed (%s): %v", u.Host, card.Reason, err)
		return card
	}
	if res.Status < 200 || res.Status > 299 {
		card.Reason = ReasonBadStatus
		return card
	}
	if signInHosts[strings.ToLower(res.URL.Hostname())] {
		card.Reason = ReasonSignIn
		return card
	}
	if res.ContentType != "" && res.ContentType != "text/html" && res.ContentType != "application/xhtml+xml" {
		card.Reason = ReasonNotHTML
		return card
	}
	meta := readPage(res.Body, res.Header.Get("Content-Type"))
	meta.fill(&card, res.URL)
	if card.SiteName == "" {
		card.SiteName = strings.TrimPrefix(strings.ToLower(res.URL.Hostname()), "www.")
	}
	// A page with nothing to say may point at its own oEmbed answer.
	if card.Title == "" && meta.oembed != "" {
		if href := absURL(res.URL, meta.oembed); href != "" {
			if o, err := fetch(ctx, href, safefetch.Options{Accept: "application/json"}); err == nil && o.Status == http.StatusOK && !o.Truncated {
				var fromOEmbed Card
				if readOEmbed(o.Body, provider{}, &fromOEmbed) {
					card.Title = fromOEmbed.Title
					card.Author = fromOEmbed.Author
					if card.ImageURL == "" {
						card.ImageURL = fromOEmbed.ImageURL
					}
				}
			}
		}
	}
	card.Fetched = true
	return card
}

// reasonFor names why a fetch failed, for the card.
//
// A name whose address is inside the network reads as unreachable, the same
// as a name with no address at all: "blocked" there would let anyone who can
// ask for a preview find out which names the server's own DNS knows
// (db.corp.example) by trying them. A link that is plainly inside (an address,
// localhost, *.internal) is refused before anything is looked up, and says so.
func reasonFor(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, safefetch.ErrResolvesInside):
		return ReasonUnreachable
	case safefetch.Refused(err):
		return ReasonBlocked
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	return ReasonUnreachable
}
