package models

import "github.com/akashc777/OneCamp/helpers"

// ForwardedPostFields and ForwardedChatFields are what a message shows of the
// message it forwards, wherever messages are read: a channel's pages and a
// thread, a DM's and a group's pages. A forward is an edge to the original,
// not a copy, so each selects the original's deletion time: a forward of a
// message deleted since shows as deleted (HideDeletedForwards), and its text,
// author and channel never leave the server. They were read with no deletion
// check, so a deleted message stayed readable wherever it had been forwarded,
// by anyone there, people who joined later included.
const ForwardedPostFields = `{
	post_uuid
	post_text
	post_created_at
	post_deleted_at
	post_by {
		user_uuid
		user_name
		user_full_name
		user_profile_object_key
		is_bot
		user_status
	}
	post_channel {
		ch_name
		ch_uuid
	}
}`

const ForwardedChatFields = `{
	chat_uuid
	chat_body_text
	chat_created_at
	chat_deleted_at
	chat_from {
		user_uuid
		user_name
		user_full_name
		user_profile_object_key
		is_bot
	}
}`

// HideDeletedForwards leaves a forwarded original that has been deleted as
// its id and its deletion time alone, which the app shows as "This message
// was deleted." The post and chat reads call it on everything they return.
func (p *DgraphPost) HideDeletedForwards() {
	if p == nil {
		return
	}
	p.ForwaredPost = deletedPostStub(p.ForwaredPost)
	p.ForwarededChat = deletedChatStub(p.ForwarededChat)
}

// HideDeletedForwards is DgraphPost's, for a DM or group message.
func (c *DgraphChat) HideDeletedForwards() {
	if c == nil {
		return
	}
	c.FwdMsgPost = deletedPostStub(c.FwdMsgPost)
	c.FwdMsgChat = deletedChatStub(c.FwdMsgChat)
}

func deletedPostStub(p *DgraphPost) *DgraphPost {
	if p == nil || !helpers.IsSoftDeleted(p.DeletedAt) {
		return p
	}
	return &DgraphPost{Uuid: p.Uuid, DeletedAt: p.DeletedAt}
}

func deletedChatStub(c *DgraphChat) *DgraphChat {
	if c == nil || !helpers.IsSoftDeleted(c.DeletedAt) {
		return c
	}
	return &DgraphChat{Uuid: c.Uuid, DeletedAt: c.DeletedAt}
}
