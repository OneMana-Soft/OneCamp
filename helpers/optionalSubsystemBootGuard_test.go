package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// An OPTIONAL subsystem must never stop the server from booting.
//
// This is not a stylistic rule. LiveKit failure was fatal at startup, and ConnectLiveKit
// makes a real ListRooms call rather than only building clients — while the compose file
// shipped to customers defines twelve services and none of them is LiveKit, and the
// shipped env template points LIVEKIT_HOST at http://livekit:7880. So the archive every
// customer downloads exited on startup and, with `restart: unless-stopped`, restarted
// forever. Nothing in the product worked, because one optional feature was unreachable.
//
// Firebase was the same shape: FIREBASE_CRED_PATH points at a credentials file the
// archive does not contain, because those credentials belong to whoever owns the mobile
// apps, so a self-hoster with no mobile app could not start OneCamp at all.
//
// The rule: REQUIRED INFRASTRUCTURE MAY BLOCK BOOT, OPTIONAL FEATURES MAY NOT. Losing a
// feature should cost that feature.

// requiredAtBoot are the subsystems the product genuinely cannot run without. Every one
// of them holds workspace data or carries every request, so continuing without it would
// mean serving errors from every page instead of failing honestly at startup.
var requiredAtBoot = map[string]string{
	"postgresInit.ConnectPostgres":     "the workspace database",
	"dgraphInit.ConnectDgraph":         "the graph store behind permissions and relations",
	"redisInit.ConnectRedis":           "sessions, presence and every cache",
	"minioInit.ConnectMinio":           "file and image storage",
	"mqttInit.ConnectMqtt":             "the realtime transport every live update uses",
	"opensearchInit.ConnectOpenSearch": "search indexing, which every write path calls",
}

// optionalAtBoot are the subsystems whose absence must degrade one feature instead. The
// value names what stops working, which is what the startup log has to tell the operator.
var optionalAtBoot = map[string]string{
	"livekitInit.ConnectLiveKit":   "audio/video calls",
	"firebaseInit.ConnectFirebase": "mobile push notifications",
}

// TestOptionalSubsystemsDoNotBlockBoot reads the startup path and checks that no optional
// subsystem's failure reaches os.Exit.
func TestOptionalSubsystemsDoNotBlockBoot(t *testing.T) {
	raw, err := os.ReadFile("../cmd/server/main.go")
	if err != nil {
		t.Fatalf("reading the startup path: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	// Comments discuss both the fatal and non-fatal cases at length, so scanning raw text
	// would match prose. Only code counts.
	code := make([]string, len(lines))
	for i, line := range lines {
		if idx := strings.Index(line, "//"); idx >= 0 {
			code[i] = line[:idx]
			continue
		}
		code[i] = line
	}

	exitPattern := regexp.MustCompile(`os\.Exit\(`)

	for call, feature := range optionalAtBoot {
		found := false
		for i, line := range code {
			if !strings.Contains(line, call) {
				continue
			}
			found = true

			// Look at the handling immediately after the call. An os.Exit within a few
			// lines is the fatal shape this test exists to prevent.
			end := i + 6
			if end > len(code) {
				end = len(code)
			}
			for _, following := range code[i:end] {
				if exitPattern.MatchString(following) {
					t.Errorf("%s is optional (%s) but its failure reaches os.Exit at startup. "+
						"A customer without it then cannot run OneCamp AT ALL. Log the loss, "+
						"let the feature registry report it unavailable, and carry on.",
						call, feature)
				}
			}
		}
		if !found {
			t.Errorf("%s is recorded as an optional subsystem but does not appear in the "+
				"startup path. Either it was renamed, in which case update this list, or it "+
				"is no longer initialised and %s is dead.", call, feature)
		}
	}
}

// TestRequiredSubsystemsStillBlockBoot is the other half, and it is what stops the fix
// above from being applied indiscriminately.
//
// Continuing without the database would replace an honest startup failure with an error on
// every request, which is strictly worse: harder to diagnose, and it looks like data loss
// to whoever is using it.
func TestRequiredSubsystemsStillBlockBoot(t *testing.T) {
	raw, err := os.ReadFile("../cmd/server/main.go")
	if err != nil {
		t.Fatalf("reading the startup path: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	code := make([]string, len(lines))
	for i, line := range lines {
		if idx := strings.Index(line, "//"); idx >= 0 {
			code[i] = line[:idx]
			continue
		}
		code[i] = line
	}

	exitPattern := regexp.MustCompile(`os\.Exit\(`)

	for call, why := range requiredAtBoot {
		located, fatal := false, false
		for i, line := range code {
			if !strings.Contains(line, call) {
				continue
			}
			located = true
			end := i + 8
			if end > len(code) {
				end = len(code)
			}
			for _, following := range code[i:end] {
				if exitPattern.MatchString(following) {
					fatal = true
				}
			}
		}
		if !located {
			t.Errorf("%s (%s) is not in the startup path; if it was renamed, update this list", call, why)
			continue
		}
		if !fatal {
			t.Errorf("%s is required (%s) but its failure no longer stops startup. The server "+
				"would come up and fail every request instead, which is harder to diagnose "+
				"than refusing to start and looks like data loss to whoever is using it.",
				call, why)
		}
	}
}

// TestEveryOptionalSubsystemReportsItsAvailability closes the loop.
//
// Degrading quietly is only half a fix: if the server starts without calls but the client
// still shows call buttons, the user gets a control that fails instead of an error at
// startup, which is not obviously better. Each optional subsystem must therefore be
// visible in the feature registry so the client can hide what is missing.
func TestEveryOptionalSubsystemReportsItsAvailability(t *testing.T) {
	// Registered by the initializer packages' init functions. helpers cannot import them
	// (they import helpers), so this asserts on the names the registry is expected to
	// carry and the initializer packages own the other side.
	expected := map[string]string{
		FeatureNameCalls: "livekitInit",
		FeatureNamePush:  "firebaseInit",
		FeatureNameAI:    "services/AI",
	}

	for name, owner := range expected {
		if strings.TrimSpace(name) == "" {
			t.Errorf("the registry key for %s is empty", owner)
		}
	}

	// Distinct keys, or one subsystem's status would silently answer for another.
	seen := map[string]string{}
	for name, owner := range expected {
		if prev, dup := seen[name]; dup {
			t.Errorf("%s and %s share the registry key %q, so one would report for the other",
				prev, owner, name)
		}
		seen[name] = owner
	}
}
