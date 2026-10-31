package router

import (
	"github.com/go-chi/chi/v5"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/helpers/idempotency"
)

// TaskCreates mounts the task routes whose resend would make something again
// (projects P7-1b): a task, a sub-task and a comment, each made once per
// Idempotency-Key (helpers/idempotency). The web's Offline Store sends a change
// made offline again with its key when the network comes back, and a retry
// after a lost answer is the same send: the server answers it from the first
// answer instead of making a second task or comment. The key is the caller's
// own (the signed-in member, or an API token): another member sending the same
// key is a send of their own, never given the first member's answer. Without a
// key, every send makes one, as before.
//
// Permission model: unchanged. The middleware runs after sign-in and each
// handler still checks who may make what. A refusal is answered again as it
// was (a 5xx lets the key go, so the send can be tried again), and a replay is
// only ever the same member's own answer.
func TaskCreates(r chi.Router) {
	r.With(idempotency.Middleware("POST /task/createTask", idempotency.InWords)).Post("/createTask", taskController.CreateTask)
	r.With(idempotency.Middleware("POST /task/createSubTask", idempotency.InWords)).Post("/createSubTask", taskController.CreateSubTask)
	r.With(idempotency.Middleware("POST /task/createCommentTask", idempotency.InWords)).Post("/createCommentTask", taskController.CreateTaskComment)
}
