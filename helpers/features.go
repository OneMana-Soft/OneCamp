package helpers

import (
	"sort"
	"sync"
)

// A registry of OPTIONAL SUBSYSTEMS and whether each one is usable right now.
//
// WHY THIS EXISTS. The frontend has to know which optional subsystems this server
// actually has, because a button that calls an endpoint that is not there is worse than
// no button. Two situations produce exactly that:
//
//   - OneCamp ships in two editions. v1 is built WITHOUT the AI packages, so its AI
//     routes do not exist at all. Its frontend must not offer AI.
//   - On v2, an operator can turn AI off (AI_ENABLED=false, or simply never configure a
//     provider). The routes exist and refuse every call.
//
// The obvious implementation has the client-config endpoint ask the AI service directly.
// That is wrong here for a structural reason: it makes a controller that BOTH editions
// need import a package that only ONE edition has, so building v1 would mean editing the
// endpoint as well as removing the subsystem. Inverting it — the subsystem announces
// itself, the endpoint only reports what announced — means the v1 build removes the AI
// packages and nothing registers, so the feature reports absent with no other change.
//
// A PROBE, NOT A BOOLEAN, because availability changes while the server runs: AI can be
// reconfigured and reloaded through the admin panel, and a value captured at startup
// would be stale the moment it was. The probe is evaluated per request.
//
// Generic on purpose. Any optional subsystem can register, and the endpoint needs no
// knowledge of which ones exist.

var (
	featureMu     sync.RWMutex
	featureProbes = map[string]func() bool{}
)

// RegisterFeature records that an optional subsystem exists, along with a probe that
// reports whether it is usable at this moment.
//
// Call it from package init, so that linking the package is what makes the feature
// visible and NOT linking it is what makes the feature absent. That is the property the
// AI-free edition relies on.
//
// Re-registering the same name replaces the probe. That is deliberate: it keeps tests
// hermetic, and a duplicate registration is a programming error that a panic would turn
// into a failed boot of the whole server for a feature flag.
func RegisterFeature(name string, probe func() bool) {
	if name == "" || probe == nil {
		return
	}
	featureMu.Lock()
	defer featureMu.Unlock()
	featureProbes[name] = probe
}

// FeatureStatus evaluates every registered probe and returns name -> usable.
//
// A probe that panics is reported as unusable rather than being allowed to take down the
// request. A feature flag endpoint is not worth a 500, and "unavailable" is the truthful
// answer about a subsystem whose own health check just crashed.
func FeatureStatus() map[string]bool {
	featureMu.RLock()
	names := make([]string, 0, len(featureProbes))
	probes := make([]func() bool, 0, len(featureProbes))
	for name, probe := range featureProbes {
		names = append(names, name)
		probes = append(probes, probe)
	}
	featureMu.RUnlock()

	status := make(map[string]bool, len(names))
	for i, name := range names {
		status[name] = safeProbe(probes[i])
	}
	return status
}

// FeatureRegistered reports whether a subsystem is COMPILED INTO this build,
// regardless of whether it is usable right now.
//
// FeatureStatus answers "can I call this at this moment", which is what a
// button needs to know. This answers "does this edition have it at all", which
// is a different question with different callers: anything baked into seeded
// data rather than read per request. The system bot's name is the case that
// forced this apart. It is written to the users table once at startup, so
// keying it on a probe would rename the bot every time an admin toggled AI
// off, and rename it back on the next boot, churning the author name on every
// message it had ever posted.
//
// The AI-free edition links no AI package, so nothing registers "ai" and this
// is false with no other change, exactly as with FeatureStatus.
func FeatureRegistered(name string) bool {
	featureMu.RLock()
	defer featureMu.RUnlock()
	_, ok := featureProbes[name]
	return ok
}

// safeProbe runs one probe, converting a panic into "unavailable".
func safeProbe(probe func() bool) (usable bool) {
	defer func() {
		if r := recover(); r != nil {
			usable = false
		}
	}()
	return probe()
}

// RegisteredFeatures lists the registered names in a stable order. Useful for tests and
// diagnostics; the order matters because Go randomises map iteration.
func RegisteredFeatures() []string {
	featureMu.RLock()
	defer featureMu.RUnlock()

	names := make([]string, 0, len(featureProbes))
	for name := range featureProbes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Registry keys for the optional subsystems.
//
// Named constants because the frontend gates on these exact strings, and a typo in either
// place fails silently by hiding the feature — the least visible kind of wrong.
const (
	// FeatureNameAI is the AI subsystem. Absent entirely on the AI-free v1 edition.
	FeatureNameAI = "ai"
	// FeatureNameCalls is audio/video calling, which needs a LiveKit server. The
	// shipped compose file does not include one, so a self-hosted install has calls
	// only if the operator runs LiveKit themselves.
	FeatureNameCalls = "calls"
	// FeatureNamePush is mobile push notification delivery, which needs Firebase
	// credentials that a self-hosted install may legitimately not have.
	FeatureNamePush = "push"
	// FeatureNameGitHub reports whether an admin has connected GitHub. It is here
	// rather than read from /admin/github/status because MEMBERS need the answer:
	// a task panel decides whether to offer a pull-request affordance with it, and
	// asking the admin endpoint made every non-admin's task panel 403.
	FeatureNameGitHub = "github"
)
