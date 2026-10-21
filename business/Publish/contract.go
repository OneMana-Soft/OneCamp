// Package business (Publish) is the server's own page for a doc anyone
// outside the workspace may read (docs C6, decision D-25): a complete,
// script-free HTML document, for a doc's view-only "Anyone with the link"
// grant reached by its token (/public/d/<token>), or by its published address
// (/public/p/<slug>). One external-access model: the grant is business/Guest's,
// with its expiry, revoke, audit and the workspace's guest-access switch; this
// package only renders what the grant opens, and copies it for a member.
//
// What a page shows: the doc's title, body and look, and nothing else of the
// workspace. Mentions of people, pages and channels are their words alone;
// links into the app lose their address; a block that shows live workspace
// data (a task list, a table, a board, a poll, a chart, a recording) isn't
// shown; pictures are this doc's own, served through the page. The page reads
// no cookie and sets none, so it is the same for everyone who holds its
// address; it says how fresh it is, with a link that reloads it.
package business

import (
	"errors"
	"time"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
)

// Ref is how a page is reached: its link's token, or its published address.
type Ref = guestBusiness.External

// Page is a page as it is served.
type Page struct {
	// Slug is its link's published address, "" when it has none.
	Slug           string
	Title          string
	HTML           string
	Indexable      bool
	AllowDuplicate bool
	UpdatedAt      time.Time
	// The doc's look: its icon when it is an emoji, its cover (a gallery
	// cover's name, or CoverURL for a picture uploaded to the doc), its font
	// ("", "serif" or "mono"), width and text size.
	Icon      string
	Cover     string
	CoverURL  string
	Font      string
	FullWidth bool
	SmallText bool
	// Token is the unlock the page's own files are asked for with, on a
	// password page; empty otherwise.
	Token string
	// DocUUID and Grant are the doc and the link it is served by, for
	// counting its visitors (never rendered).
	DocUUID string
	Grant   string
}

// File is one of a page's files, as anyone with the page may have it.
type File struct {
	ObjKey string
	// ETag names the file: a file kept with a doc never changes.
	ETag      string
	Indexable bool
	// Private is a password page's file: no shared cache may keep it.
	Private bool
}

var (
	// ErrNotFound is a page that isn't there, or isn't for anyone outside:
	// the same answer either way.
	ErrNotFound = errors.New("publish: not found")
	// ErrForbidden is someone who may not do that (an agent copying a page).
	ErrForbidden = errors.New("publish: not allowed")
	// ErrGone is a published page whose link has expired.
	ErrGone = errors.New("publish: the page has expired")
	// ErrLocked is a password page asked for without its unlock.
	ErrLocked = errors.New("publish: the page needs its password")
	// ErrWrongPassword is a password that doesn't open the page.
	ErrWrongPassword = errors.New("publish: that password doesn't open the page")
	// ErrBusy is a page asked to do more work in a minute than it does.
	ErrBusy = errors.New("publish: the page is busy")
	// ErrNoCopies is a page whose link doesn't let members copy it.
	ErrNoCopies = errors.New("publish: its owner doesn't let it be copied")
)
