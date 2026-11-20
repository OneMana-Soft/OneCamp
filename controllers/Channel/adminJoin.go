package controllers

// CR-405 N2: a workspace admin who joins a channel (the moderation door V-1163
// keeps open) leaves one admin-audit row, whatever the channel's size and
// whether or not it says who joined.

import (
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
)

// Seams for tests: whether the asker acts as a workspace admin, and the
// audit writer.
var (
	actingAdmin = func(r *http.Request) bool {
		_, who := accessBusiness.RequestSubject(r)
		return who.ActingAdmin()
	}
	recordedJoin = auditBusiness.Recorded
)

// joinRecorded runs join, an asker joining the channel channelUUID, named
// name; for a workspace admin, under one audit row written first.
func joinRecorded(r *http.Request, channelUUID, name string, join func() error) error {
	if !actingAdmin(r) {
		return join()
	}
	return recordedJoin(r, "channel.admin.join", auditBusiness.CategorySecurity, "Joined a channel as a workspace admin",
		map[string]interface{}{"channel_uuid": channelUUID, "channel_name": name}, join)
}
