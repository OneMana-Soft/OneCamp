//go:build integration

package business

import "net/http"

// ForwardToThisNetworkForTest lets commands and card presses reach an app on
// this machine, over plain http, until restore is called. It exists only in
// the integration build: the tag keeps it out of the shipped binary.
//
// The integration test of response URLs and card presses serves its app with
// httptest, on loopback; which addresses OneCamp forwards to is tested where
// that rule is the subject (helpers/ssrf_test.go).
func ForwardToThisNetworkForTest() (restore func()) {
	oldCheck, oldClient := forwardCheck, forwardClient
	forwardCheck = func(string) error { return nil }
	forwardClient = func() *http.Client { return &http.Client{} }
	return func() { forwardCheck, forwardClient = oldCheck, oldClient }
}
