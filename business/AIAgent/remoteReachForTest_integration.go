//go:build integration

package business

import (
	"context"

	"github.com/google/uuid"
)

// LetRemotesReachThisNetworkForTest makes every agent's remote brain count as
// one an admin added, so it may be on this server's own network, until restore
// is called. It exists only in the integration build: the tag keeps it out of
// the shipped binary.
//
// An integration test about something else (what an agent may read for whoever
// asked it), whose agent's sponsor is a member, serves its scripted brain on
// loopback. Which addresses a remote may reach, and that a member's may not be
// on this network, is tested where that rule is the subject
// (mcp_secret_binding_test, agentRemote_test), so turning it off here hides
// nothing that test is about.
//
// Why a hook rather than a setting: there is no setting for this. Who added a
// remote decides where it may connect (creatorIsAdmin), and the operator-wide
// switch that once let every remote reach this network was removed on purpose,
// so nothing in the environment can stand in for that decision. This package's
// own tests set creatorIsAdmin directly (main_test.go); a test in another
// package can reach it only through here.
func LetRemotesReachThisNetworkForTest() (restore func()) {
	old := creatorIsAdmin
	creatorIsAdmin = func(context.Context, *uuid.UUID) bool { return true }
	return func() { creatorIsAdmin = old }
}
