package business

import (
	"context"
	"slices"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	"github.com/google/uuid"
)

// The kinds of item ArchiveFilesOf takes files off with: a channel's post or
// a conversation's or group's message, each with its thread's replies, and a
// comment.
var (
	PostFiles    = domain.Post
	ChatFiles    = domain.Chat
	CommentFiles = domain.Comment
)

// Where ArchiveFilesOf reads and writes: seams.
var (
	archiveHeldFiles = domain.ArchiveHeldFiles
	rowsOf           = domain.Rows
	liveHolders      = domain.LiveHolders
	importNodesOf    = domain.ImportNodes
	archiveRows      = domain.ArchiveRows
)

// requestsNamedFilesUntil is when the last write that kept the files its
// request named stopped (2687eb418, 19 Oct 2026: posts, conversation and
// group messages and thread replies keep only their writer's own uploads to
// that conversation; forwards had stopped hours before). Until then a write
// kept any file id its request carried, and a forward wrote its nodes with
// no adder or time.
var requestsNamedFilesUntil = time.Date(2026, time.October, 19, 12, 44, 52, 0, time.FixedZone("IST", 5*60*60+30*60))

// ArchiveFilesOf takes the files a person deleted along with the item that
// held them, at at: the item's own and its replies'. Their graph nodes are
// stamped deleted, and so is each file's attachments row, unless an item
// still there holds the file. Every read by id then answers that the file
// isn't there, and the purge takes it. It answers how many rows it stamped.
//
// A file is one row however many items show it, so the row stays while one
// of these holds it:
//   - an item the server paired with the deleted one: a reply sent to its
//     channel too is the reply and its copy there (post_broadcast_of);
//   - an item to which the server attached the file for its own uploader: a
//     node whose adder, which the server records, is the row's uploader. A
//     writer can attach only their own uploads to the place they went to
//     (OwnFiles), as when its uploader forwards a message within its place;
//   - an item written before requestsNamedFilesUntil, whatever it shows: its
//     node is older, or has no time. Until then items shared a row with no
//     link the server made: a reply also sent to its channel before the copy
//     was linked (13 Oct), a forward to several places at once (a node
//     naming the one upload in each), a forward within a place. Taking the
//     file off would take it from the item still there. Such an item naming
//     someone else's file keeps that file open to the people of the file's
//     place (GAPS P-FILE-4L, the residual main accepted).
//
// A file's id in a node written since holds nothing: an id is the client's
// to write, so another person's message naming it could otherwise keep a
// deleted file alive.
//
// A file an import made holds by the same rules, but for two of the import's
// own (main's ruling, 26 Oct; CODE-REVIEWER-50's MF1'). Its file stages wrote
// each file twice: the node the item it recorded as the file's holder lists,
// and before it one more with the very same time that nothing lists (the
// Slack stage gave both the export's time of their message). That other node
// is the import's leftover: once the listed node is stamped deleted, it holds
// nothing, whichever item went, the import's own or a forward of it after.
// And a node is never older than the import that made its file (the file's
// row's time), so an export's time older than 19 Oct isn't the time a node
// was written. See holds.
//
// Deleting a post, a message or a comment used to mark its files deleted in
// search only: everyone who could read the place kept opening them by id, and
// the purge, which takes only stamped rows, never took them. Nothing restores
// a deleted post, message or comment, so nothing needs its files back.
//
// Run again after it failed, it finishes the job: it answers for every file
// the item holds, stamped before or not.
func ArchiveFilesOf(ctx context.Context, holder domain.Holder, id uuid.UUID, at time.Time) (int, error) {
	held, err := archiveHeldFiles(ctx, holder, id, at)
	if err != nil {
		return 0, err
	}
	open := slices.DeleteFunc(slices.Clone(held.Files), func(f uuid.UUID) bool { return held.Paired[f] })
	if len(open) == 0 {
		return 0, nil
	}
	rows, err := rowsOf(ctx, open)
	if err != nil {
		return 0, err
	}
	holders, err := liveHolders(ctx, open)
	if err != nil {
		return 0, err
	}
	imports, err := importNodesOf(ctx, open)
	if err != nil {
		return 0, err
	}
	return archiveRows(ctx, slices.DeleteFunc(open, func(f uuid.UUID) bool {
		listed, imported := imports[f]
		return slices.ContainsFunc(holders[f], func(n domain.HolderNode) bool { return holds(n, rows[f], imported, listed) })
	}), at)
}

// holds is whether the live node n holds its file, whose row is r: it was
// written before requestsNamedFilesUntil, or with no time, or by the row's
// uploader, as the server records it. For a file an import made, whose items
// the import recorded list the nodes listed:
//   - a node with the very time of a listed node stamped deleted, written
//     before it, is the import's other node of the file, which nothing lists,
//     and holds nothing. A node a person or the server wrote after the import
//     has a time of its own, and one that carries a listed node's time was
//     written after it;
//   - a node is no older than the row the import made for its file, so an
//     export's time isn't taken for the time the node was written.
func holds(n domain.HolderNode, r domain.Row, imported bool, listed []domain.ImportNode) bool {
	at := n.At
	if imported && !at.IsZero() {
		for _, l := range listed {
			if l.Stamped && n.Uid < l.Uid && at.Equal(l.At) {
				return false
			}
		}
		if at.Before(r.At) {
			at = r.At
		}
	}
	return at.Before(requestsNamedFilesUntil) || r.By != uuid.Nil && n.By[r.By]
}
