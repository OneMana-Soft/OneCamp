package business

import (
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// A DM or group call rings when its first person really joins (call.started),
// and says the same words it always has.
func TestAChatCallRingsWhenItReallyStarts(t *testing.T) {
	for kind, want := range map[string]bool{"dm": true, "group": true, "channel": false, "meeting": false} {
		if got := chatCallStarted(resourceEventBusiness.Event{Kind: resourceEventBusiness.CallStarted, ResourceKind: kind}); got != want {
			t.Errorf("a %s call starting rings DMs and groups: %v, want %v", kind, got, want)
		}
	}
	if chatCallStarted(resourceEventBusiness.Event{Kind: resourceEventBusiness.CallJoined, ResourceKind: "dm"}) {
		t.Error("a join after the first rang again")
	}
	if title, body := chatCallPush("Maya", false); title != "Call from Maya" || body != "calling you..." {
		t.Errorf("a DM call says %q %q", title, body)
	}
	if title, body := chatCallPush("Maya", true); title != "Maya started group call" || body != "started call" {
		t.Errorf("a group call says %q %q", title, body)
	}
}
