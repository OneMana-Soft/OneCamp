package business

import (
	"testing"
	"time"
)

// A place's file is never signed for long (a member who loses the place loses
// its files within minutes), and a kept redirect never outlives its signature.
func TestMediaLinksExpireInsideTheirSignatures(t *testing.T) {
	if PlaceFileURLTTL > 5*time.Minute {
		t.Errorf("a place's file is signed for %s; keep it to five minutes at most", PlaceFileURLTTL)
	}
	if PlaceFileRedirectMaxAge >= PlaceFileURLTTL || AvatarRedirectMaxAge >= AvatarURLTTL {
		t.Error("a kept redirect would point at an expired address")
	}
}
