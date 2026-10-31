package docController

// A link in the doc tree (docs F1): made through POST /doc/createDoc, called
// by its address, and named after its page's preview once that is read
// (business/Unfurl, through safefetch), unless someone named it first. The
// preview is read after the node is made, so making a link never waits on
// another site, and a preview that can't be read leaves the address.

import (
	"context"
	"errors"
	"strings"
	"time"

	business "github.com/akashc777/OneCamp/business/Doc"
	unfurlBusiness "github.com/akashc777/OneCamp/business/Unfurl"
	"github.com/akashc777/OneCamp/helpers"
)

// Seams for tests: what makes a doc, and what names a new link.
var (
	makeDoc       = business.CreateDocInTree
	nameLinkLater = nameLinkFromPreview
)

// linkNamingTimeout bounds a link's naming: the preview (Unfurl's own 8 s)
// and the write.
const linkNamingTimeout = 15 * time.Second

// maxLinkTitle bounds a link's title as a file's name is bounded.
const maxLinkTitle = 255

// errLinkFetches is a person over their fetches for the minute: their link
// keeps its address as its title.
var errLinkFetches = errors.New("controllers: too many links previewed in a minute")

// linkTitle is what a link is called after its preview: its page's title,
// else its site's name; "" when the preview says neither, which leaves it
// called by its address. Pure.
func linkTitle(card unfurlBusiness.Card) string {
	if !card.Fetched {
		return ""
	}
	title := strings.TrimSpace(card.Title)
	if title == "" {
		title = strings.TrimSpace(card.SiteName)
	}
	return helpers.TruncateRunes(title, maxLinkTitle)
}

// nameLinkFromPreview reads a new link's preview in the background, counted
// in its maker's fetches for the minute as a preview the app asks for is
// (unfurlBusiness.AllowFetch), and names the link after it while it is still
// called by its address.
func nameLinkFromPreview(ctx context.Context, byUUID, docUUID, address string) {
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("doc.link-title", func() {
		ctx, cancel := context.WithTimeout(ctx, linkNamingTimeout)
		defer cancel()
		budget := func() error {
			if ok, _ := unfurlBusiness.AllowFetch(ctx, "link", byUUID); !ok {
				return errLinkFetches
			}
			return nil
		}
		card, err := unfurlBusiness.Unfurl(ctx, address, budget)
		if err != nil {
			helpers.LogInfoWithContext(ctx, "controllers/nameLinkFromPreview: link %s keeps its address as its title: %v", docUUID, err)
			return
		}
		title := linkTitle(card)
		if title == "" {
			return
		}
		if err := business.NameLink(ctx, docUUID, address, title, business.Actor{UserUUID: byUUID}); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/nameLinkFromPreview: naming link %s: %v", docUUID, err)
		}
	})
}
