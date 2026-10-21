package business

import (
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// A channel's call is announced when its first person really joins, and only
// a channel's.
func TestAChannelCallIsAnnouncedWhenItReallyStarts(t *testing.T) {
	if !channelCallStarted(resourceEventBusiness.Event{Kind: resourceEventBusiness.CallStarted, ResourceKind: "channel"}) {
		t.Error("a channel's call starting wasn't announced")
	}
	for _, e := range []resourceEventBusiness.Event{
		{Kind: resourceEventBusiness.CallStarted, ResourceKind: "dm"},
		{Kind: resourceEventBusiness.CallJoined, ResourceKind: "channel"},
		{Kind: resourceEventBusiness.CallEnded, ResourceKind: "channel"},
	} {
		if channelCallStarted(e) {
			t.Errorf("%s of a %s call was announced as a channel call starting", e.Kind, e.ResourceKind)
		}
	}
}
