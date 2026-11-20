package domain

// Which way a page of a conversation's messages runs (D5, H5B). A channel's,
// a DM's and a group's pages were each four copies of one query, one per
// direction, that differed only in their block's filter and order; each kind
// now has one page query (domain/Chat ChatPage, domain/Post PostPage), and
// this is where the four directions differ.

import "github.com/akashc777/OneCamp/helpers/dgraphquery"

// Direction is which way a page runs from its edge, the time $time.
type Direction int

const (
	// Latest is the newest messages, newest first; it has no edge.
	Latest Direction = iota
	// Older is the messages before the edge, newest first.
	Older
	// Newer is the messages after the edge, oldest first.
	Newer
	// NewerFrom is the messages at or after the edge, oldest first: the
	// page that opens at a linked message.
	NewerFrom
)

// Block is a page's block arguments over messages whose time is the predicate
// created and whose deletion time is deleted: the live ones on d's side of
// $time, the first $first of them in d's order.
func (d Direction) Block(created, deleted string) string {
	live := dgraphquery.Live(deleted)
	switch d {
	case Older:
		return "@filter(lt(" + created + ", $time) AND " + live + ")(first: $first, orderdesc: " + created + ")"
	case Newer:
		return "@filter(gt(" + created + ", $time) AND " + live + ")(first: $first, orderasc: " + created + ")"
	case NewerFrom:
		return "@filter(ge(" + created + ", $time) AND " + live + ")(first: $first, orderasc: " + created + ")"
	default:
		return "@filter(" + live + ")(first: $first, orderdesc: " + created + ")"
	}
}
