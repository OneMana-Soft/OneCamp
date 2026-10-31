package business

// A member's read of their own Slack conversations (member_read.go) sits in
// the import history beside the admins' imports, and
// what it holds is the member's. The admins see that it happened: who brought
// it, when, how far it got and how many conversations. They never see what's
// in it: its plan, its notes, its failure's words and the names its run noted
// all name the member's conversations. These say which jobs are such reads and
// what of one each viewer is shown. Pure, so every screen of the history (the
// Slack import's and the import page's) answers the same, and kept here rather
// than with the models, whose every call is a store's.

import (
	"encoding/json"

	"github.com/google/uuid"
)

// MemberReadRefused is why nobody plans, runs or retries a member's read by
// hand: it plans and runs itself, as its member, when its read ends.
const MemberReadRefused = "This is a member bringing their own Slack DMs. It plans and runs by itself, and only they can bring them again."

// memberReadHiddenFailure is a member's read's failure as everyone else reads
// it.
const memberReadHiddenFailure = "It didn't finish. Why is shown to them alone, as it can name their conversations."

// MemberRead reports whether a job's options mark it as a member's read of
// their own Slack conversations.
func MemberRead(options json.RawMessage) bool {
	var o struct {
		MemberDMs bool `json:"member_dms"`
	}
	return len(options) > 0 && json.Unmarshal(options, &o) == nil && o.MemberDMs
}

// ContentsShownTo reports whether viewer sees what a job holds: a member's read
// is shown to its member alone, any other job to everyone who sees the history.
func ContentsShownTo(options json.RawMessage, triggeredBy *uuid.UUID, viewer uuid.UUID) bool {
	return !MemberRead(options) || (triggeredBy != nil && *triggeredBy == viewer)
}

// HiddenFailure is a member's read's failure as someone who isn't its member
// is shown it: that it failed, without why. Nil stays nil.
func HiddenFailure(msg *string) *string {
	if msg == nil {
		return nil
	}
	hidden := memberReadHiddenFailure
	return &hidden
}

// HiddenProgress is a member's read's progress as someone who isn't its
// member is shown it: how far the read got, in counts (progress.read), and
// none of the names its plan and run noted beside them.
func HiddenProgress(progress json.RawMessage) json.RawMessage {
	var all map[string]json.RawMessage
	if len(progress) == 0 || json.Unmarshal(progress, &all) != nil {
		return nil
	}
	var read struct {
		Users         int   `json:"users"`
		Groups        int   `json:"groups"`
		DMs           int   `json:"dms"`
		MPIMs         int   `json:"mpims"`
		Conversations int   `json:"conversations"`
		Of            int   `json:"of"`
		Messages      int   `json:"messages"`
		Replies       int   `json:"replies"`
		Files         int   `json:"files"`
		Bytes         int64 `json:"bytes"`
	}
	if raw, ok := all["read"]; !ok || json.Unmarshal(raw, &read) != nil {
		return json.RawMessage(`{}`)
	}
	out, err := json.Marshal(map[string]any{"read": read})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}
