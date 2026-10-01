package livekitInit

import (
	"testing"
)

// TestConnectLiveKitFailsWhenTheServerIsAbsent documents the behaviour that made the
// shipped stack unbootable.
//
// ConnectLiveKit does not merely build clients, it makes a real ListRooms call to
// LIVEKIT_HOST. The shipped compose file defines twelve services and none of them is
// LiveKit, while the shipped env template points LIVEKIT_HOST at http://livekit:7880.
// So on a stock customer install this call could never succeed — and main.go treated the
// error as fatal, so the backend exited and, with `restart: unless-stopped`, restarted
// forever.
//
// Calls are an OPTIONAL subsystem. This test exists so that the failure stays a fact the
// code acknowledges: the error must be returned (so the caller can decide to continue
// without calls) and must never be assumed away.
func TestConnectLiveKitFailsWhenTheServerIsAbsent(t *testing.T) {
	// A host that cannot resolve, which is exactly what "livekit" is on a stack with no
	// LiveKit service.
	cfg := &LiveKitConfigStruct{
		HostURL:   "http://livekit-does-not-exist.invalid:7880",
		ApiKey:    "devkey",
		ApiSecret: "devsecretdevsecretdevsecretdevsecret",
	}

	err := ConnectLiveKit(cfg)
	if err == nil {
		t.Fatal("ConnectLiveKit reported success against a host that does not exist. If it " +
			"no longer verifies reachability, the startup path must be revisited: the whole " +
			"point of treating calls as optional is that this check can fail.")
	}
	t.Logf("unreachable LiveKit correctly reported: %v", err)
}
