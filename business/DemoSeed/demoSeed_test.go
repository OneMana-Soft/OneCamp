package business

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// The curated content must not read like the mash it replaces.
//
// The demo was a workspace of "yo", "fdf", "zhh" and "horseys" in channels called
// dsfsdfsdfdf and jkkjj kjkjkj, shown to every buyer of a product whose pitch is
// that it is careful. Replacing that with generated filler would be the same
// mistake in better handwriting, so the content is checked for the properties
// that made the old content embarrassing.
func TestCuratedContentReadsLikeAWorkspace(t *testing.T) {
	if len(Curated) == 0 {
		t.Fatal("no curated content, so this seeder produces an empty workspace")
	}
	channelName := regexp.MustCompile(`^[a-z][a-z0-9-]{2,}$`)

	for _, conv := range Curated {
		if !channelName.MatchString(conv.Channel) {
			t.Errorf("channel %q is not a plain lowercase handle; the demo's old names "+
				"were the first thing that made it look abandoned", conv.Channel)
		}
		if len(conv.Posts) < 2 {
			t.Errorf("#%s has %d message(s); a channel with one message reads as seeded, "+
				"not used", conv.Channel, len(conv.Posts))
		}
		for i, line := range conv.Posts {
			p := line.Text
			words := len(strings.Fields(p))
			if words < 8 {
				t.Errorf("#%s message %d is %d words (%q). The old demo was full of "+
					"two-word messages and that is exactly what looked unfinished.",
					conv.Channel, i+1, words, p)
			}
			if !strings.ContainsAny(p, ".?!") {
				t.Errorf("#%s message %d is not a sentence: %q", conv.Channel, i+1, p)
			}
		}
	}
}

// Archiving must be driven by a written-down list, never by a pattern.
//
// A regular expression that decides what looks like junk will eventually archive
// a channel somebody meant to keep, and on a customer workspace that is data loss
// dressed as tidying. The names here were observed on the demo host and are
// reviewable in a diff.
func TestJunkListIsExplicitAndDisjointFromTheCuration(t *testing.T) {
	if len(Junk) == 0 {
		t.Fatal("the junk list is empty, so nothing is ever cleaned up")
	}
	curated := map[string]bool{}
	for _, c := range Curated {
		curated[c.Channel] = true
	}
	for _, j := range Junk {
		if curated[j] {
			t.Errorf("%q is both curated and junk, so one run would create it and the "+
				"next would archive it", j)
		}
	}
	seen := map[string]bool{}
	for _, j := range Junk {
		if seen[j] {
			t.Errorf("%q appears twice in the junk list", j)
		}
		seen[j] = true
	}
}

// The content must not promise anything the product does not do, because this is
// shown to buyers and a demo that oversells is worse than one that is empty.
func TestCuratedContentMakesNoClaimsAboutRefunds(t *testing.T) {
	for _, conv := range Curated {
		for _, line := range conv.Posts {
			p := line.Text
			if strings.Contains(strings.ToLower(p), "refund") {
				t.Errorf("#%s mentions refunds: %q. OneMana does not offer them and the "+
					"demo must not imply otherwise.", conv.Channel, p)
			}
		}
	}
}

// A channel with one voice is a seeded database, however good the sentences are.
//
// The first version of this seeder posted everything as the admin, so each of the
// three channels held one person talking to themselves for four messages: tidier
// than the keyboard mash it replaced and just as obviously fake. Whoever reads the
// demo is deciding whether real people use this product.
func TestEveryChannelIsAConversation(t *testing.T) {
	for _, conv := range Curated {
		speakers := map[int]bool{}
		for _, line := range conv.Posts {
			speakers[line.Speaker] = true
		}
		if len(speakers) < 2 {
			t.Errorf("#%s has %d speaker(s) across %d messages; one person posting four "+
				"times in a row reads as seeded, not used", conv.Channel, len(speakers), len(conv.Posts))
		}
		// And the same person must not simply alternate with themselves at the
		// ends: somebody has to answer somebody.
		if len(conv.Posts) > 2 && conv.Posts[0].Speaker == conv.Posts[1].Speaker &&
			conv.Posts[1].Speaker == conv.Posts[2].Speaker {
			t.Errorf("#%s opens with three messages from the same speaker", conv.Channel)
		}
	}
}

// Speaker indices must stay within a cast a small workspace can actually supply.
// They wrap, so an out-of-range index is not a crash — it is one voice silently
// becoming another, which is worse.
func TestSpeakerIndicesFitASmallCast(t *testing.T) {
	const castSize = 3
	for _, conv := range Curated {
		for i, line := range conv.Posts {
			if line.Speaker < 0 || line.Speaker >= castSize {
				t.Errorf("#%s message %d uses speaker %d; the cast is %d people and the "+
					"index wraps, so this silently becomes somebody else",
					conv.Channel, i+1, line.Speaker, castSize)
			}
		}
	}
}

// A refresh has to free the name, not just hide the channel.
//
// ch_name is UNIQUE. The first refresh archived the curated channels and stopped
// there, so the name stayed taken, the seeder could not create the replacement,
// and the run left the demo with no curated channels at all — worse than the
// state it was asked to improve. It was found by running it against the real
// workspace, which is the only place a unique constraint speaks up.
func TestRetiredNameFreesTheOriginalAndIsUnique(t *testing.T) {
	at := time.Unix(1757800000, 0)
	for _, conv := range Curated {
		retired := retireName(conv.Channel, at)
		if retired == conv.Channel {
			t.Fatalf("retiring #%s did not change its name, so the seeder cannot create "+
				"the replacement", conv.Channel)
		}
		if !strings.HasPrefix(retired, conv.Channel+"-") {
			t.Errorf("retired name %q does not keep the original recognisable", retired)
		}
		// Handle-safe, or the rename fails validation and nothing is retired.
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(retired) {
			t.Errorf("retired name %q is not a usable channel handle", retired)
		}
	}
	// Two retirements at different times must not collide with each other.
	if retireName("general", at) == retireName("general", at.Add(time.Second)) {
		t.Error("two retirements produce the same name, so the second one fails on the " +
			"unique constraint")
	}
}
