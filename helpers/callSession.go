package helpers

import "strings"

// A call that nobody records still produces speech worth keeping.
//
// Transcript lines hang off a recording node, which is keyed by the egress id
// of the recording that produced them. That is why an unrecorded call had no
// transcript: there was no egress id, so there was no key, so there was nowhere
// to put the words. The agent gave up before sending them and the API would
// have rejected them anyway.
//
// A call session key is that missing key. It is derived from the room's session
// id, which LiveKit issues per call rather than per room, so it names exactly
// "this call" and a later call in the same channel gets a different one.
const callSessionPrefix = "call-"

// CallSessionKeyFor builds the key from a room's session id.
//
// Deleted once, when the transcription agent was the only thing that built
// these keys and the dead-exported-function guard correctly said so. Browser
// mode brought a second producer, and this one runs on the server, so the
// construction belongs here rather than being spelled out a third time at a
// call site. Returns "" for an empty session id rather than a bare prefix,
// which would collide across every call that hit the same bug.
func CallSessionKeyFor(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	return callSessionPrefix + sessionID
}

// IsCallSessionKey reports whether a key names a call session rather than a
// recording.
//
// This is the one place that knows the difference, because two things downstream
// depend on getting it right: such a node must never appear in a list of
// recordings a human can play, and a recap must not offer a "play recording"
// button for a call where there is no recording to play.
func IsCallSessionKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), callSessionPrefix)
}
