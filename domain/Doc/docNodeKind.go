package domain

// The nodes of the doc tree that aren't pages (docs F1, Docs & Files): a
// folder, which holds pages, folders, files and links; a file, whose bytes are
// attachments rows and whose versions are in Postgres (doc_file_versions); and
// a link to a document elsewhere (a Drive, OneDrive or Google document). Each
// is a doc node like a page: one access rule, one trash, the same stars,
// search and history. doc_node_kind says which; a page has none, so nothing
// ever writes "" or "page".
//
// What follows from that (design/docs-vs-notion/F1-FILES.md, "What has to
// change"):
//   - only a page opens in the editor: the collaboration service's reads and
//     saves refuse the rest (ErrNotAPage);
//   - every scan across docs reads pages only (pageOnly): the doc repair, the
//     AI index's access backfill and embeddings, backlinks, the retention's
//     inactivity archive, the work graph's link reconciler and the export;
//   - search gives a folder or a link an entry with its title alone, and finds
//     a file through its versions' entries in the attachments index;
//   - only a page or a folder holds children.
//
// A kind, a colour and a link are checked here before anything is stored
// (CheckNodeKind, CheckColor, CheckLinkURL), and helpers' guards hold the
// rest: nothing writes a kind outside this set, and the reads that must take
// pages only carry pageOnly.

import (
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The kinds of node that aren't pages.
const (
	NodeFolder = "folder"
	NodeFile   = "file"
	NodeLink   = "link"
)

// nodeKinds is every kind a node may be written with.
var nodeKinds = []string{NodeFolder, NodeFile, NodeLink}

// pageOnly keeps pages: a page never has a kind (doc_node_kind's exact index
// answers has() from its posting list). The one spelling of the filter, so a
// read that takes pages only names it rather than writing it again.
const pageOnly = `NOT has(doc_node_kind)`

// PageOnly is pageOnly for the reads outside this package that take pages
// only: the work graph's reconciler reads doc bodies for links
// (domain/WorkGraph refs.go).
const PageOnly = pageOnly

// LeafFilter holds of a node that holds no others: a file or a link (D-30).
// The doc tree's schema names it (business/Doc DocTree), so a node made or
// moved under one is refused (business/Tree/graph Siblings and Apply). It
// filters one node read by its uuid, never a root: the kind's index lists
// every folder and file there is.
const LeafFilter = `eq(doc_node_kind, "` + NodeFile + `") OR eq(doc_node_kind, "` + NodeLink + `")`

// nodeLook is what a read that shows a node takes of it beside its kind,
// which every decision read carries (accessScalars): its colour and a link's
// address.
const nodeLook = `
	doc_color
	doc_link_url`

// maxLinkURL bounds a link's address, as a link preview's is
// (business/Unfurl).
const maxLinkURL = 2048

var (
	// ErrNotAPage is something only a page does, asked of a folder, a file or
	// a link: the editor's session, a save, a body.
	ErrNotAPage = errors.New("doc: that isn't a page")
	// ErrBadNodeKind is a kind that isn't folder, file or link.
	ErrBadNodeKind = errors.New("doc: no such kind of node")
	// ErrBadColor is a colour that isn't one of the camp's hues.
	ErrBadColor = errors.New("doc: no such colour")
	// ErrBadLink is a link's address that isn't a public https address.
	ErrBadLink = errors.New("doc: a link must be an https address")
)

// IsNodeKind is whether k is a kind a node may be written with.
func IsNodeKind(k string) bool { return slices.Contains(nodeKinds, k) }

// IsPage is whether a doc is a page: it has no kind. doc must be read with
// doc_node_kind (every row and decision read is: docRowFields,
// accessScalars), or a folder reads as a page.
func IsPage(doc *dgraphStruct.DgraphDoc) bool {
	return doc != nil && doc.NodeKind == ""
}

// HoldsChildren is whether a node may have nodes under it: a page or a
// folder; a file and a link are leaves (D-30). doc must be read with
// doc_node_kind. A node's kind never changes once it is made, so a check of a
// parent read a moment before is still true when the node under it is made.
func HoldsChildren(doc *dgraphStruct.DgraphDoc) bool {
	return doc != nil && (doc.NodeKind == "" || doc.NodeKind == NodeFolder)
}

// CheckNodeKind is nil for folder, file or link, and ErrBadNodeKind for
// anything else, "" and "page" included: a page is written with no kind.
func CheckNodeKind(k string) error {
	if IsNodeKind(k) {
		return nil
	}
	return ErrBadNodeKind
}

// CheckColor is nil for one of the camp's hues (helpers.CampHues) and
// ErrBadColor for anything else. No colour is none written, never "".
func CheckColor(c string) error {
	if helpers.IsCampHue(c) {
		return nil
	}
	return ErrBadColor
}

// CheckLinkURL is a link's address as it is stored, or ErrBadLink: an https
// address only (never http, javascript:, data: or a relative one), at most
// maxLinkURL long, with no user:password@, to a host that is a public address
// or a name that can mean something outside the network (safefetch.CheckURL:
// the shared check, which looks nothing up). The host comes back lower-cased
// and in ASCII.
func CheckLinkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxLinkURL {
		return "", ErrBadLink
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return "", ErrBadLink
	}
	checked, err := safefetch.CheckURL(raw)
	if err != nil || checked.Scheme != "https" {
		return "", ErrBadLink
	}
	out := checked.String()
	if len(out) > maxLinkURL {
		return "", ErrBadLink
	}
	return out, nil
}

// checkNodeFields checks a node about to be written: its kind, if it has one;
// its colour, if it has one; and a link's address, which a link must have and
// nothing else may.
func checkNodeFields(doc *dgraphStruct.DgraphDoc) error {
	if doc == nil {
		return nil
	}
	if doc.NodeKind != "" {
		if err := CheckNodeKind(doc.NodeKind); err != nil {
			return err
		}
	}
	if doc.Color != "" {
		if err := CheckColor(doc.Color); err != nil {
			return err
		}
	}
	switch {
	case doc.NodeKind == NodeLink:
		link, err := CheckLinkURL(doc.LinkURL)
		if err != nil {
			return err
		}
		doc.LinkURL = link
	case doc.LinkURL != "":
		return ErrBadLink
	}
	return nil
}
