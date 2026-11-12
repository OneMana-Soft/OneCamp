package business

import "github.com/akashc777/OneCamp/helpers"

// Where the workspace runs, as every screen that says so reads it: the
// sign-in page's trust line (/auth/providers), the signed-in app
// (/config/client) and the plan (/admin/plan). One answer, so no two screens
// can disagree about whose server this is.
const (
	HostingDemo  = "demo"  // the public demo, shared by everyone who opens it
	HostingCloud = "cloud" // OneCamp Cloud runs it (Managed)
	HostingSelf  = "self"  // the customer's own server
)

// Hosting is where this workspace runs. The demo comes first: whoever runs
// it, a visitor must be told it is a shared demo.
func Hosting() string {
	if helpers.DemoMode() {
		return HostingDemo
	}
	if Managed() {
		return HostingCloud
	}
	return HostingSelf
}
