package livekitInit

// Companions (calls meet-04, PROGRAM.md B1): another device of someone in a
// call, the way Meet's companion mode is. A phone or a second tab joins for
// the chat, the hands and the reactions, or to present a screen, without the
// person's microphone, camera or speaker, so the call never hears them twice.
//
// One person, one identity: LiveKit lets one connection hold an identity and
// drops the older when another joins with it (DUPLICATE_IDENTITY), which is
// what Switch here is. A companion therefore has an identity of its own,
// "<graph uid>~c~<tag>", the tag drawn by the server, never by a client, and
// it reads back as its person's (CompanionOf, PersonOf) wherever who someone
// is matters: their part, whether they may stay, being put out.
//
// What a companion may publish is decided here, where every call token is
// minted and where what someone may publish is changed: a screen and its
// sound, never a microphone or a camera, whatever it is asked to grant.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	livekit "github.com/livekit/protocol/livekit"
)

// companionMark separates a person's graph uid from their companion's tag.
const companionMark = "~c~"

// companionTagBytes is how much randomness a companion's tag carries (12 hex
// characters), so two devices of one person never share an identity.
const companionTagBytes = 6

// CompanionSources is all a companion may ever publish: a screen, with its
// sound. Never a microphone or a camera.
var CompanionSources = []string{"screen_share", "screen_share_audio"}

// ErrNoCompanion is a companion asked for someone who can't have one: only a
// member, by their graph uid, has companions.
var ErrNoCompanion = errors.New("livekitInit: only a member has companions")

// NewCompanionIdentity draws the identity of a new companion of person (a
// member's graph uid), with a tag from crypto/rand.
func NewCompanionIdentity(person string) (string, error) {
	if !helpers.IsDgraphUID(person) {
		return "", ErrNoCompanion
	}
	tag := make([]byte, companionTagBytes)
	if _, err := rand.Read(tag); err != nil {
		return "", err
	}
	return person + companionMark + hex.EncodeToString(tag), nil
}

// CompanionOf answers whose companion identity is, when it is one: a member's
// graph uid, the mark, and a tag of the server's shape. Pure.
func CompanionOf(identity string) (person string, ok bool) {
	person, tag, found := strings.Cut(identity, companionMark)
	if !found || !helpers.IsDgraphUID(person) || len(tag) != 2*companionTagBytes {
		return "", false
	}
	if _, err := hex.DecodeString(tag); err != nil || strings.ToLower(tag) != tag {
		return "", false
	}
	return person, true
}

// PersonOf is who an identity is in a call: a companion's person, or the
// identity itself. Pure.
func PersonOf(identity string) string {
	if person, ok := CompanionOf(identity); ok {
		return person
	}
	return identity
}

// DevicesOf is person's connections among people: their companions, and
// their own connection when own. With own, it is everyone whose PersonOf is
// person, so a companion's identity, which is nobody's person, has none.
// Pure.
func DevicesOf(people []*livekit.ParticipantInfo, person string, own bool) []*livekit.ParticipantInfo {
	var out []*livekit.ParticipantInfo
	for _, p := range people {
		if p == nil {
			continue
		}
		of, ok := CompanionOf(p.Identity)
		if (ok && of == person) || (own && !ok && p.Identity == person) {
			out = append(out, p)
		}
	}
	return out
}

// IsCompanion is whether someone connected to a call is a companion of a
// person in it. Pure.
func IsCompanion(p *livekit.ParticipantInfo) bool {
	if p == nil {
		return false
	}
	_, ok := CompanionOf(p.Identity)
	return ok
}

// publishableBy is what identity may publish of sources: a companion only its
// CompanionSources among them, anyone else all of them. Pure.
func publishableBy(identity string, sources []string) []string {
	if _, ok := CompanionOf(identity); !ok {
		return sources
	}
	return slices.DeleteFunc(slices.Clone(sources), func(s string) bool { return !slices.Contains(CompanionSources, s) })
}
