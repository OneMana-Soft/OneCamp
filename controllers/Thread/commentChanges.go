package controllers

// Changes to a comment already written, on a kind whose comments threads keep
// (business/Thread commentChanges.go: a table's rows): edit your own, delete
// your own or, as a moderator, anyone's, and react. A doc's comments change
// through the doc's own comment routes, and are answered "not available" here.
// An admin removing someone else's comment, which only being an admin allowed,
// is recorded in the audit log.

import (
	"net/http"

	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

func commentID(r *http.Request) string { return chi.URLParam(r, "comment_id") }

// EditThreadComment gives a comment its writer's new words.
// POST /threads/{thread_id}/comments/{comment_id}/edit {body}
func EditThreadComment(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in threadBusiness.Body
	if !decode(w, r, &in) {
		return
	}
	c, err := threadBusiness.Comments.Edit(r.Context(), who, threadID(r), commentID(r), in)
	if err != nil {
		write(w, r, "EditThreadComment", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": c})
}

// RemoveThreadComment takes a comment from every reader.
// POST /threads/{thread_id}/comments/{comment_id}/remove
func RemoveThreadComment(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	removed, err := threadBusiness.Comments.Remove(r.Context(), who, threadID(r), commentID(r))
	if err != nil {
		write(w, r, "RemoveThreadComment", err)
		return
	}
	if removed.ByAdmin {
		adminAuditBusiness.Record(r, "thread.comment.removed", adminAuditBusiness.CategorySecurity,
			"Removed someone else's comment, as an admin", map[string]interface{}{"thread_id": threadID(r), "comment_id": commentID(r)})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
}

// ReactToThreadComment puts the member's reaction on a comment, or changes
// its emoji. POST /threads/{thread_id}/comments/{comment_id}/react {emoji_id}
func ReactToThreadComment(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in struct {
		EmojiID string `json:"emoji_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, err := threadBusiness.Comments.React(r.Context(), who, threadID(r), commentID(r), in.EmojiID)
	if err != nil {
		write(w, r, "ReactToThreadComment", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"reaction_id": id}})
}

// UnreactToThreadComment takes back the member's own reaction.
// POST /threads/{thread_id}/comments/{comment_id}/unreact {reaction_id}
func UnreactToThreadComment(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in struct {
		ReactionID string `json:"reaction_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := threadBusiness.Comments.Unreact(r.Context(), who, threadID(r), commentID(r), in.ReactionID); err != nil {
		write(w, r, "UnreactToThreadComment", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
}
