package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"google.golang.org/api/googleapi"
)

// The inbox page decides what to show from these: the connect button for
// not_connected, a reconnect button for reconnect, otherwise the message.
func TestInboxFailTellsThePageWhatToDo(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"never connected", connectorBusiness.ErrNotConnected, http.StatusConflict, "not_connected"},
		{"revoked", fmt.Errorf("gmail threads: %w", &googleapi.Error{Code: 401}), http.StatusConflict, "reconnect"},
		{"missing scope", &googleapi.Error{Code: 403, Message: "insufficient authentication scopes"}, http.StatusConflict, "reconnect"},
		{"bad id", connectorBusiness.ErrBadThreadID, http.StatusBadRequest, ""},
		{"empty reply", connectorBusiness.ErrEmptyReply, http.StatusBadRequest, ""},
		{"injected header", connectorBusiness.ErrBadHeader, http.StatusBadRequest, ""},
		{"deleted thread", &googleapi.Error{Code: 404}, http.StatusNotFound, ""},
		{"quota", &googleapi.Error{Code: 429}, http.StatusTooManyRequests, ""},
		{"api off", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured"}}}, http.StatusServiceUnavailable, ""},
		{"outage", errors.New("connection reset"), http.StatusBadGateway, ""},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		InboxFail(w, httptest.NewRequest(http.MethodGet, "/", nil), c.err, "test")
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.status)
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if got, _ := body["code"].(string); got != c.code {
			t.Errorf("%s: code %q, want %q", c.name, got, c.code)
		}
		if msg, _ := body["msg"].(string); msg == "" {
			t.Errorf("%s: no message for the person", c.name)
		}
	}
}
