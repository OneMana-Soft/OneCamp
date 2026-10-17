package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
)

// writeGuestRead answers a guest's read: what the link opens, and whose
// workspace it is (workspace_name). Every read a guest page opens with or
// draws from goes through here (the meeting's status, a doc's or board's live
// session, a table, a channel and its threads, a project and its tasks, a
// doc's guest comments), so the page's frame names the same workspace
// wherever it was opened. One place, so a guest read added later gets the
// name by answering through it.
//
// Only an answer that opens something: a link that is off, revoked, expired
// or gone answers before this (notAvailable), with nothing of the workspace.
func writeGuestRead(w http.ResponseWriter, data any) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": withWorkspaceName(data, settingsBusiness.WorkspaceName())})
}

// guestRead is a guest answer's data with the workspace's name beside its own
// fields, which the web app reads as one object ("" when the server has no
// name: the page then uses its own build's).
type guestRead struct {
	data          any
	workspaceName string
}

func withWorkspaceName(data any, name string) guestRead {
	return guestRead{data: data, workspaceName: name}
}

// MarshalJSON writes the data's own object with "workspace_name" added as its
// first field, the data encoded once (a table's rows are not decoded and
// encoded again to add one field). Data that isn't an object is written as it
// is, without the name: there is no field to add it beside.
func (g guestRead) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(g.data)
	if err != nil {
		return nil, err
	}
	body = bytes.TrimSpace(body)
	if len(body) < 2 || body[0] != '{' {
		return body, nil
	}
	name, err := json.Marshal(g.workspaceName)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+len(name)+20)
	out = append(out, `{"workspace_name":`...)
	out = append(out, name...)
	if rest := bytes.TrimSpace(body[1 : len(body)-1]); len(rest) > 0 {
		out = append(out, ',')
		out = append(out, rest...)
	}
	return append(out, '}'), nil
}
