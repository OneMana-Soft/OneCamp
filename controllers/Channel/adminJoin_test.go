package controllers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAWorkspaceAdminsJoinWritesOneAuditRow(t *testing.T) {
	prevAdmin, prevRec := actingAdmin, recordedJoin
	t.Cleanup(func() { actingAdmin, recordedJoin = prevAdmin, prevRec })
	var rows []string
	var meta []map[string]interface{}
	recordedJoin = func(_ *http.Request, action, _, _ string, m map[string]interface{}, change func() error) error {
		rows = append(rows, action)
		meta = append(meta, m)
		return change()
	}
	r := httptest.NewRequest(http.MethodPost, "/channel/join", nil)
	joined := 0
	join := func() error { joined++; return nil }

	actingAdmin = func(*http.Request) bool { return true }
	if err := joinRecorded(r, "ch-1", "launch", join); err != nil || joined != 1 || len(rows) != 1 || rows[0] != "channel.admin.join" {
		t.Fatalf("admin: rows %v joined %d err %v", rows, joined, err)
	}
	if meta[0]["channel_uuid"] != "ch-1" {
		t.Fatalf("meta %v", meta[0])
	}
	// No row, no join.
	recordedJoin = func(*http.Request, string, string, string, map[string]interface{}, func() error) error {
		return errors.New("not recorded")
	}
	if err := joinRecorded(r, "ch-1", "launch", join); err == nil || joined != 1 {
		t.Fatalf("unrecorded join ran: %d %v", joined, err)
	}
	// A member's own join writes none.
	rows = nil
	recordedJoin = prevRec
	actingAdmin = func(*http.Request) bool { return false }
	if err := joinRecorded(r, "ch-1", "launch", join); err != nil || joined != 2 || len(rows) != 0 {
		t.Fatalf("member: rows %v joined %d err %v", rows, joined, err)
	}
}
