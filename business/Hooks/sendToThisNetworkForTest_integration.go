//go:build integration

package business

import (
	"net/http"
	"time"
)

// SendToThisNetworkForTest lets deliveries reach an endpoint on this machine,
// over plain http, until restore is called. It exists only in the integration
// build: the tag keeps it out of the shipped binary.
//
// The integration test of deliveries (that they survive a failure and a
// restart, keep their event id, pause, resume and replay) serves its endpoint
// with httptest, on loopback; which addresses OneCamp sends to is tested where
// that rule is the subject (helpers/ssrf_test.go), so turning it off here hides
// nothing this test is about.
func SendToThisNetworkForTest() (restore func()) {
	oldClient, oldCheck := client, checkURL
	client = &http.Client{Timeout: sendTimeout + time.Second}
	checkURL = func(string) error { return nil }
	return func() { client, checkURL = oldClient, oldCheck }
}
