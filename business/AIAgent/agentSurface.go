package business

// Reply-surface descriptor for durable agent runs (async-mentions spec Task 2).
//
// A durable agent job (ai_agent_tasks) today always targets a project task. To
// let the SAME durable engine drive a channel post thread, a group chat, or a
// 1:1 DM, a job carries a small, generic Surface descriptor: the kind of reply
// surface + the ids needed to post/edit the agent's status comment there. The
// worker selects a per-surface status poster from Kind, so adding a new surface
// later is a new poster, not a change to the worker loop.
//
// This file is PURE (encode/decode only) so it is unit-testable without a DB,
// and it degrades safely: a blank/malformed descriptor decodes to the legacy
// task surface, so an older job row (no descriptor) keeps working unchanged.

import (
	"encoding/json"
	"strings"
)

// SurfaceKind identifies where a durable run posts its status/result.
type SurfaceKind string

const (
	// SurfaceTask is the legacy task-assignment surface (status posted as a
	// comment on the assigned project task). The safe default.
	SurfaceTask SurfaceKind = "task"
	// SurfaceChannelPost posts the status as an in-thread comment on a channel
	// post (a channel/thread @mention).
	SurfaceChannelPost SurfaceKind = "channel_post"
	// SurfaceGroupChat posts the status as an in-thread comment on a group-chat
	// message.
	SurfaceGroupChat SurfaceKind = "group_chat"
	// SurfaceDM posts the status as an in-thread comment on a 1:1 DM message.
	SurfaceDM SurfaceKind = "dm"
)

// validSurfaceKind reports whether k is a known surface kind.
func validSurfaceKind(k SurfaceKind) bool {
	switch k {
	case SurfaceTask, SurfaceChannelPost, SurfaceGroupChat, SurfaceDM:
		return true
	default:
		return false
	}
}

// Surface is the reply-surface descriptor persisted on a durable job. Only the
// ids relevant to Kind are populated. It is intentionally a flat, additive
// shape so a new surface adds a field without breaking older encoded rows.
type Surface struct {
	Kind SurfaceKind `json:"kind"`
	// ChannelPost: the channel + the triggering/parent post the reply threads on.
	ChannelID string `json:"channel_id,omitempty"`
	PostID    string `json:"post_id,omitempty"`
	// GroupChat / DM: the chat grouping + the triggering message the reply
	// threads on.
	GroupID   string `json:"group_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// EncodeSurface serializes a Surface for storage on a job (JSON). A zero/invalid
// Kind is normalized to the legacy task surface so a caller can never persist a
// nonsensical descriptor.
func EncodeSurface(s Surface) (string, error) {
	if !validSurfaceKind(s.Kind) {
		s.Kind = SurfaceTask
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeSurface parses a stored descriptor. It NEVER errors: a blank blob (an
// older task-assignment job that predates the descriptor) or a malformed/unknown
// one decodes to the legacy task surface, so the worker's default path keeps
// working. Ids are trimmed.
func DecodeSurface(raw string) Surface {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Surface{Kind: SurfaceTask}
	}
	var s Surface
	if err := json.Unmarshal([]byte(raw), &s); err != nil || !validSurfaceKind(s.Kind) {
		return Surface{Kind: SurfaceTask}
	}
	s.ChannelID = strings.TrimSpace(s.ChannelID)
	s.PostID = strings.TrimSpace(s.PostID)
	s.GroupID = strings.TrimSpace(s.GroupID)
	s.MessageID = strings.TrimSpace(s.MessageID)
	return s
}
