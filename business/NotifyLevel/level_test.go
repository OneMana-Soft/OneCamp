package notifylevel

import (
	"slices"
	"testing"
)

// Each level against each kind of notice: "Everything" hears all of them,
// "Mentions" all but a plain message, "Nothing" none (an @mention of them
// included), and any other value none (no setting at all is Hears': it hears).
func TestEachLevelLetsWhatItSays(t *testing.T) {
	for _, c := range []struct {
		level                string
		plain, thread, named bool
	}{
		{"all", true, true, true},
		{"mention", false, true, true},
		{"block", false, false, false},
		{"", false, false, false},
		{"none", false, false, false},
	} {
		for h, want := range map[Heard]bool{Plain: c.plain, Thread: c.thread, Named: c.named} {
			if got := Lets(c.level, h); got != want {
				t.Errorf("Lets(%q, %d) = %v, want %v", c.level, h, got, want)
			}
		}
	}
}

func TestHearingKeepsWhomTheirLevelLets(t *testing.T) {
	levels := map[string]string{"ada": "block", "ben": "mention", "cy": "all", "dee": "mention"}
	named := map[string]bool{"ada": true, "ben": true}
	got := Hearing(levels, []string{"ada", "ben", "cy", "dee", "eve"}, func(id string) Heard {
		if named[id] {
			return Named
		}
		return Plain
	})
	// Ada muted it, Dee hears only what names her, Eve has no setting here and
	// hears: only a mute removes.
	if want := []string{"ben", "cy", "eve"}; !slices.Equal(got, want) {
		t.Fatalf("Hearing = %v, want %v", got, want)
	}
}
