package controllers

// A member bringing their own Slack DMs, group DMs and private channels into
// the team's import (business/SlackImport member_read.go), from their
// settings. Every route here is a signed-in session's (router/chatSlackDMs.go
// mounts them behind the member group, which takes no API token); a request
// an API token or an agent asked for is refused here as well, since a token
// would let anyone holding it bring a person's DMs in as them.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	importAdapter "github.com/akashc777/OneCamp/adapter/SlackImport"
	business "github.com/akashc777/OneCamp/business/SlackImport"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// memberReadStatus is the answer to each refusal of a member's read, by its
// code (business.MemberReadRefusal).
var memberReadStatus = map[string]int{
	"bad_token":      http.StatusBadRequest,
	"no_email":       http.StatusBadRequest,
	"slack_refused":  http.StatusBadRequest,
	"not_your_token": http.StatusForbidden,
	"no_team_import": http.StatusConflict,
	"other_slack":    http.StatusConflict,
	"under_way":      http.StatusConflict,
	"not_yours":      http.StatusNotFound,
}

// mustMember is the signed-in member asking for themselves, or false with the
// request answered: never work an API token or an agent asked for.
func mustMember(w http.ResponseWriter, r *http.Request) (*userModel.UserInfo, bool) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return nil, false
	}
	if helpers.ActsThroughToken(r.Context()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":  "Bring your Slack DMs from OneCamp itself, signed in as you. An API token or an agent can't.",
			"code": "session_only",
		})
		return nil, false
	}
	return &u, true
}

// writeMemberReadError answers a member's read refused, in its words, or a
// failure of the server's own as one.
func writeMemberReadError(w http.ResponseWriter, r *http.Request, err error, failed string) {
	if code, msg, ok := business.MemberReadRefusal(err); ok {
		helpers.WriteJSON(w, memberReadStatus[code], helpers.Envolope{"msg": msg, "code": code})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "SlackImport member read: %s: %+v", failed, err)
	helpers.WriteError(w, http.StatusServiceUnavailable, failed+" Try again in a minute.")
}

// HandleMyRead starts the signed-in member's read of their own Slack DMs,
// group DMs and private channels with their own token, into the team's
// import. Body: {"token": "xoxp-…", "since": "YYYY-MM-DD" (optional)}. The
// token is used for the read and kept nowhere.
func HandleMyRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	member, ok := mustMember(w, r)
	if !ok {
		return
	}
	if res := checkSlackImportRateLimit(ctx, "my-read", member.UserPostgresInfo.Id, 3); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	var body struct {
		Token string `json:"token"`
		Since string `json:"since"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		helpers.WriteBadRequest(w, "invalid body")
		return
	}
	opts := business.APIReadOptions{MaxBytes: slackUploadCap()}
	if s := strings.TrimSpace(body.Since); s != "" {
		since, err := time.Parse("2006-01-02", s)
		if err != nil {
			helpers.WriteBadRequest(w, "since is a date: YYYY-MM-DD")
			return
		}
		opts.Since = since
	}
	jobId, err := business.StartMemberRead(ctx, member, body.Token, opts)
	if err != nil {
		writeMemberReadError(w, r, err, "Couldn't start bringing your DMs.")
		return
	}
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"job_id": jobId})
}

// HandleMyReads is the signed-in member's own reads, newest first, each as
// the import history shows a job, all of it theirs to see, and whether their
// team has imported Slack (none: there is nothing to bring their DMs into).
func HandleMyReads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	member, ok := mustMember(w, r)
	if !ok {
		return
	}
	jobs, teamImported, err := business.MemberReads(ctx, member.UserPostgresInfo.Id)
	if err != nil {
		writeMemberReadError(w, r, err, "Couldn't load your reads.")
		return
	}
	ids := make([]uuid.UUID, len(jobs))
	for i, j := range jobs {
		ids[i] = j.Id
	}
	counts, _ := business.JobCounts(ctx, ids)
	views := make([]importAdapter.JobView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, jobToView(j, counts[j.Id], member.UserPostgresInfo.Id, nil))
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"reads": views, "team_imported": teamImported})
}

// HandleMyReadNotes is a page of what one of the member's reads noted.
func HandleMyReadNotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	member, ok := mustMember(w, r)
	if !ok {
		return
	}
	jobId, validID := helpers.UUIDParam(w, r, "jobId")
	if !validID {
		return
	}
	limit := helpers.LimitParam(r.URL.Query(), 100, 500)
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	notes, err := business.MemberReadNotes(ctx, member.UserPostgresInfo.Id, jobId, limit, max(offset, 0))
	if err != nil {
		writeMemberReadError(w, r, err, "Couldn't load what the read noted.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"notes": notes})
}

// HandleStopMyRead stops one of the member's reads; what has come across
// stays.
func HandleStopMyRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	member, ok := mustMember(w, r)
	if !ok {
		return
	}
	jobId, validID := helpers.UUIDParam(w, r, "jobId")
	if !validID {
		return
	}
	err := business.StopMemberRead(ctx, member.UserPostgresInfo.Id, jobId)
	if err != nil && !errors.Is(err, business.ErrNotYourRead) {
		// It ended before the stop came: nothing to stop.
		if j, getErr := business.GetJob(ctx, jobId); getErr == nil && j != nil && !jobStoppable(j.Status) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That read has already ended.", "code": "ended"})
			return
		}
	}
	if err != nil {
		writeMemberReadError(w, r, err, "Couldn't stop the read.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// jobStoppable is whether a job in status can still be stopped.
func jobStoppable(status string) bool {
	switch status {
	case importModels.StatusPending, importModels.StatusValidating, importModels.StatusPlanned,
		importModels.StatusRunning, importModels.StatusPaused:
		return true
	}
	return false
}
