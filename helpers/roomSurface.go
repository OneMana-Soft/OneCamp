package helpers

// Which surface a LiveKit room belongs to.
//
// A room name is overloaded: for a channel call it IS the channel uuid, and for
// a direct or group chat it is the canonical grouping id (see GetGroupingId).
// Telling them apart is three lines and was written out inline wherever it was
// needed, which is how two callers end up disagreeing about what a room is.
//
// It lives in helpers rather than beside either caller because both editions
// need it: the workflow engine ships on the AI-free build and cannot import the
// AI package that first had this logic.

import "strings"

// ClassifyRoom returns the channel uuid OR the chat grouping id for a room, and
// empty strings for a room that is neither.
//
// Exactly one of the two is non-empty for a real conversation. An instant
// meeting (meet-<uuid>) belongs to no conversation at all, so both come back
// empty and a caller can tell there is no surface to act on rather than
// guessing.
func ClassifyRoom(roomName string) (channelUUID, chatGroupID string) {
	name := strings.TrimSpace(roomName)
	if name == "" {
		return "", ""
	}

	switch {
	case strings.Contains(name, " "):
		// A grouping id is space-joined sorted user uuids, so a space is the
		// one character that cannot appear in any other kind of room name.
		return "", name
	case !strings.Contains(name, "-") && len(name) == 32:
		// An unhyphenated 32-character name is a uuid with its dashes stripped,
		// which is how a two-person chat grouping id is written.
		return "", name
	case strings.HasPrefix(name, "meet-"):
		// An instant meeting is not attached to a channel or a chat. Naming it
		// here rather than letting it fall through means a caller gets "no
		// surface" instead of a channel uuid that does not exist.
		return "", ""
	default:
		return name, ""
	}
}
