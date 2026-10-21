package controllers

// Anchored threads over HTTP (business/Thread; routes in router/docsThreads.go,
// members only). Every answer is {"data": …} or {"msg": …} with a message
// written for the person.

import (
	"encoding/json"
	"errors"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"io"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// bodyLimit is the most a thread request carries: a 256 KiB comment and its
// attachments' details.
const bodyLimit = 512 << 10

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var invalid *threadBusiness.Invalid
	var refusal *threadBusiness.Refusal
	switch {
	case errors.As(err, &invalid):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Msg})
	case errors.As(err, &refusal):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": refusal.Msg})
	case errors.Is(err, threadBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That thread or page no longer exists."})
	case errors.Is(err, threadBusiness.ErrGone):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "That page is in the trash."})
	case errors.Is(err, threadBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't do that here."})
	case errors.Is(err, threadBusiness.ErrUnavailable):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Comments aren't available here."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Thread/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Something went wrong.")
	}
}

// subjectOf is the signed-in member, as the threads service decides for them.
func subjectOf(w http.ResponseWriter, r *http.Request) (accessBusiness.Subject, bool) {
	_, who := accessBusiness.RequestSubject(r)
	if who.UserUUID == "" {
		helpers.WriteUnauthorized(w)
		return accessBusiness.Subject{}, false
	}
	return who, true
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return false
	}
	return true
}

func threadID(r *http.Request) string { return chi.URLParam(r, "thread_id") }

// ListThreads is a resource's threads with their comments.
// GET /threads?resource_kind=doc&resource_uuid=…&status=open|resolved
func ListThreads(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	status := threadBusiness.Status(q.Get("status"))
	if status != "" && status != threadBusiness.StatusOpen && status != threadBusiness.StatusResolved {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Show open or resolved threads."})
		return
	}
	res := accessBusiness.Resource{Kind: q.Get("resource_kind"), UUID: q.Get("resource_uuid")}
	if res.Kind == "" || res.UUID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which page's threads to show."})
		return
	}
	list, err := threadBusiness.Threads.List(r.Context(), who, res, threadBusiness.Filter{Status: status})
	if err != nil {
		write(w, r, "ListThreads", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"threads": list}})
}

// GetThread is one thread with its comments. GET /threads/{thread_id}
func GetThread(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	v, err := threadBusiness.Threads.Get(r.Context(), who, threadID(r))
	if err != nil {
		write(w, r, "GetThread", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// CreateThread starts a thread with its first comment.
// POST /threads {resource_kind, resource_uuid, anchor, body, attachments}
func CreateThread(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in struct {
		ResourceKind string                `json:"resource_kind"`
		ResourceUUID string                `json:"resource_uuid"`
		Anchor       threadBusiness.Anchor `json:"anchor"`
		threadBusiness.Body
	}
	if !decode(w, r, &in) {
		return
	}
	res := accessBusiness.Resource{Kind: in.ResourceKind, UUID: in.ResourceUUID}
	if res.Kind == "" || res.UUID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which page to comment on."})
		return
	}
	v, err := threadBusiness.Threads.Create(r.Context(), who, res, in.Anchor, in.Body)
	if err != nil {
		write(w, r, "CreateThread", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// ReplyToThread adds a comment. POST /threads/{thread_id}/replies {body, attachments}
func ReplyToThread(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in threadBusiness.Body
	if !decode(w, r, &in) {
		return
	}
	c, err := threadBusiness.Threads.Reply(r.Context(), who, threadID(r), in)
	if err != nil {
		write(w, r, "ReplyToThread", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": c})
}

// ResolveThread resolves a thread. POST /threads/{thread_id}/resolve
func ResolveThread(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	t, err := threadBusiness.Threads.Resolve(r.Context(), who, threadID(r))
	if err != nil {
		write(w, r, "ResolveThread", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// ReopenThread opens a resolved thread again. POST /threads/{thread_id}/reopen
func ReopenThread(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	t, err := threadBusiness.Threads.Reopen(r.Context(), who, threadID(r))
	if err != nil {
		write(w, r, "ReopenThread", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// ReanchorThread moves a thread to a new anchor, on someone's "Reattach
// here". POST /threads/{thread_id}/reanchor {anchor}
func ReanchorThread(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in struct {
		Anchor threadBusiness.Anchor `json:"anchor"`
	}
	if !decode(w, r, &in) {
		return
	}
	t, err := threadBusiness.Threads.Reanchor(r.Context(), who, threadID(r), in.Anchor)
	if err != nil {
		write(w, r, "ReanchorThread", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// MarkThreadRead records that the member has read a thread.
// POST /threads/{thread_id}/read
func MarkThreadRead(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	if err := threadBusiness.Threads.MarkRead(r.Context(), who, threadID(r)); err != nil {
		write(w, r, "MarkThreadRead", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
}

// LinkThreadTask is POST /threads/{thread_id}/task {task_uuid}: ties a thread
// to the task "Make a task" made from it (docs C5-2), for someone who may
// resolve the thread and see the task. From then on resolving either resolves
// both (business/Task threadTaskSync.go).
func LinkThreadTask(w http.ResponseWriter, r *http.Request) {
	who, ok := subjectOf(w, r)
	if !ok {
		return
	}
	var in struct {
		TaskUUID string `json:"task_uuid"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || in.TaskUUID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which task."})
		return
	}
	task, err := taskBusiness.GetDgraphBasicTaskInfo(r.Context(), in.TaskUUID, who.UserUID)
	if err != nil || task == nil || !taskBusiness.CanViewTask(task, who.UserUID) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That task isn't here."})
		return
	}
	t, err := threadBusiness.LinkTask(r.Context(), who, threadID(r), task.Uuid)
	if errors.Is(err, threadBusiness.ErrTiedElsewhere) {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This comment already has a task."})
		return
	}
	if err != nil {
		write(w, r, "LinkThreadTask", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}
