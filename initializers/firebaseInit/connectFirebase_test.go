package firebaseInit

import (
	"context"
	"testing"

	"firebase.google.com/go/v4/messaging"
)

// TestMultiCastPushIsQuietWhenFirebaseIsAbsent is the send-path half of
// treating push as optional.
//
// Startup no longer exits when credentials are missing, so the messaging client
// stays nil. Almost every chat/comment/task path still calls MultiCastPush.
// A nil dereference there would turn "no mobile app" into a panic on the
// next message, which is the same outage we just removed from boot.
func TestMultiCastPushIsQuietWhenFirebaseIsAbsent(t *testing.T) {
	saved := FirebaseApp.Messaging()
	FirebaseApp.setMessaging(nil)
	t.Cleanup(func() { FirebaseApp.setMessaging(saved) })

	err := FirebaseApp.MultiCastPush(context.Background(), map[string]string{
		FIREBASE_PUSH_DATA_TITLE: "n",
		FIREBASE_PUSH_DATA_BODY:  "b",
	}, []string{"token"})
	if err != nil {
		t.Fatalf("push with no Firebase client must be a no-op, got %v", err)
	}
}

// A credential can now be replaced while the process runs, so the swap must be
// safe to do concurrently with the sends that read it. Run under -race this is
// the test that fails if the guard is ever removed.
func TestClientCanBeSwappedWhileBeingRead(t *testing.T) {
	saved := FirebaseApp.Messaging()
	t.Cleanup(func() { FirebaseApp.setMessaging(saved) })

	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			FirebaseApp.setMessaging(nil)
		}
		close(done)
	}()
	for i := 0; i < 2000; i++ {
		_ = Available()
	}
	<-done
}

// Not configured is a supported state, not a failure: a self-hoster with no
// mobile app has no use for Firebase and must still be able to boot.
func TestNoCredentialIsNotAnError(t *testing.T) {
	saved := FirebaseApp.Messaging()
	t.Cleanup(func() { FirebaseApp.setMessaging(saved) })

	if err := ConnectFirebase(&FirebaseAppConfigStruct{FirebaseCredJSON: "   "}); err != nil {
		t.Fatalf("an empty credential should disable push, not fail: %v", err)
	}
	if Available() {
		t.Fatal("push reports available with no credential")
	}
}

// A bad credential must not take down a working one. This is what makes the
// admin screen safe to use on a live workspace.
func TestARejectedCredentialLeavesTheOldOneInPlace(t *testing.T) {
	saved := FirebaseApp.Messaging()
	t.Cleanup(func() { FirebaseApp.setMessaging(saved) })

	// Stand in for a loaded client. Only its non-nil-ness is under test.
	FirebaseApp.setMessaging(&messaging.Client{})

	if err := ConnectFirebase(&FirebaseAppConfigStruct{FirebaseCredJSON: `{"type":"service_account"}`}); err == nil {
		t.Fatal("a credential with no key was accepted")
	}
	if !Available() {
		t.Fatal("a rejected credential turned off push that was working")
	}
}
