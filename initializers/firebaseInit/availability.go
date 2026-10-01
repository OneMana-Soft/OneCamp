package firebaseInit

import (
	"github.com/akashc777/OneCamp/helpers"
)

// Push notification delivery is an OPTIONAL subsystem.
//
// It needs Firebase service-account credentials, which a self-hosted install may
// legitimately not have: the shipped env template points FIREBASE_CRED_PATH at
// firebase-cred.json, a file the archive does not contain, because those credentials
// belong to whoever owns the mobile apps. Startup treated a missing or invalid credential
// file as FATAL, so a self-hoster who never intends to ship a mobile app could not start
// the server at all.
//
// Losing push should cost push. Everything else in the product works without it, and the
// client can simply stop offering notification settings that could never take effect.

// Available reports whether push delivery can be attempted.
//
// No network probe. Unlike LiveKit, the question here is whether credentials LOADED, which
// is decided once when they are read; a Firebase API call would test Google's
// availability rather than this deployment's configuration, and would fail transiently for
// reasons that have nothing to do with whether the operator set push up.
func Available() bool {
	return FirebaseApp.Messaging() != nil
}

// init announces push delivery to the feature registry that /config/client reports.
func init() {
	helpers.RegisterFeature(helpers.FeatureNamePush, Available)
}
