package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	triageMail "github.com/akashc777/OneCamp/business/TriageMail"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
)

const mailboxAdminsOnly = "Only the project's admins connect its mailbox."

// mailboxUnreachable is the one answer for a server this server can't reach
// and one it may not (on its own network): the same status and words, so a
// project admin can't map the network's names by testing them (CR-409 N2).
const mailboxUnreachable = "The mail server couldn't be reached from here. Check the host and port; a mail server on this server's own network is connected by a workspace admin."

// writeMailErr answers a mailbox route's error in words an admin can act on;
// the server's own words never reach the answer or a log.
func writeMailErr(w http.ResponseWriter, err error) {
	status, msg := http.StatusInternalServerError, "The mailbox couldn't be saved. Try again."
	switch {
	case errors.Is(err, triageMail.ErrNotAdmin):
		status, msg = http.StatusForbidden, mailboxAdminsOnly
	case errors.Is(err, triageMail.ErrThroughToken):
		status, msg = http.StatusForbidden, "A mailbox is connected in the app, not through a token."
	case errors.Is(err, triageMail.ErrNoMailbox):
		status, msg = http.StatusNotFound, "This project has no mailbox."
	case errors.Is(err, triageMail.ErrNoPassword):
		status, msg = http.StatusBadRequest, "Enter the mailbox's password."
	case errors.Is(err, triageMail.ErrBox):
		status, msg = http.StatusBadRequest, "Enter a host, and use TLS on port 993 or STARTTLS on port 143."
	case errors.Is(err, triageMail.ErrAuth):
		status, msg = http.StatusUnprocessableEntity, "The mail server refused that username and password."
	case errors.Is(err, triageMail.ErrTLS):
		status, msg = http.StatusUnprocessableEntity, "The mail server's secure connection failed."
	case errors.Is(err, triageMail.ErrRefused):
		status, msg = http.StatusUnprocessableEntity, mailboxUnreachable
	default:
		helpers.MessageLogs.ErrorLog.Printf("controllers/Triage mailbox: %T", err)
		var ne net.Error
		if errors.As(err, &ne) {
			status, msg = http.StatusUnprocessableEntity, mailboxUnreachable
		}
	}
	helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
}

// readSettings reads a mailbox's settings (8 KB at most).
func readSettings(w http.ResponseWriter, r *http.Request) (triageMail.Settings, bool) {
	var in triageMail.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return in, false
	}
	return in, true
}

// Mailbox handles GET /project/{project_uuid}/triage/mailbox: the project's
// mailbox for its admins, "connected as" its account; never its password.
func Mailbox(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedManage, mailboxAdminsOnly)
	if !ok {
		return
	}
	v, err := triageMail.Read(r.Context(), s, projectID)
	if err != nil {
		writeMailErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// SetMailbox handles PUT /project/{project_uuid}/triage/mailbox {host, port,
// tls, username, password, folder, paused}: an admin connects the project's
// mailbox, changes, pauses or resumes it. Recorded in the audit log (never
// the password).
func SetMailbox(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedManage, mailboxAdminsOnly)
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	in, ok := readSettings(w, r)
	if !ok {
		return
	}
	v, err := triageMail.Save(r.Context(), &me, s, projectID, in)
	if err != nil {
		writeMailErr(w, err)
		return
	}
	auditBusiness.Record(r, "triage.mailbox.save", auditBusiness.CategoryIntegration, "Connected a project's mailbox to its triage",
		map[string]interface{}{"project_uuid": projectID.String(), "host": v.Host, "connected_as": v.ConnectedAs, "paused": in.Paused})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v, "msg": "Saved."})
}

// RemoveMailbox handles DELETE /project/{project_uuid}/triage/mailbox: an
// admin disconnects the project's mailbox (soft-deleted, its secret wiped).
func RemoveMailbox(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedManage, mailboxAdminsOnly)
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	if err := triageMail.Remove(r.Context(), &me, s, projectID); err != nil {
		writeMailErr(w, err)
		return
	}
	auditBusiness.Record(r, "triage.mailbox.remove", auditBusiness.CategoryIntegration, "Disconnected a project's mailbox",
		map[string]interface{}{"project_uuid": projectID.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Disconnected."})
}

// TestMailbox handles POST /project/{project_uuid}/triage/mailbox/test: logs
// in and opens the folder, nothing fetched or kept.
func TestMailbox(w http.ResponseWriter, r *http.Request) {
	projectID, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedManage, mailboxAdminsOnly)
	if !ok {
		return
	}
	me, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	in, ok := readSettings(w, r)
	if !ok {
		return
	}
	if err := triageMail.Test(r.Context(), &me, s, projectID, in); err != nil {
		writeMailErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Connected."})
}
