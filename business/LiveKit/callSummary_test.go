package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/tests/livekitfake"
)

// V-951. A call's summary (the AI's meeting recap) is said to be made only of
// a transcript the call keeps, and only while the workspace makes them. With
// nobody to say whether it does, as in the edition without AI, none is said.
// The AI is never asked about a transcript that isn't kept.
func TestASummaryIsSaidOnlyOfAKeptTranscriptWhileTheWorkspaceMakesThem(t *testing.T) {
	ctx := context.Background()
	if makesSummary(ctx, &TranscriptionSwitch{On: true, Kept: true}) {
		t.Fatal("nothing said recaps are made (the edition without AI), yet a summary was")
	}
	defer func(was func(context.Context) bool) { recapMade = was }(recapMade)
	for _, c := range []struct {
		name             string
		made, kept, want bool
		asked            int
	}{
		{"kept, recaps on", true, true, true, 1},
		{"kept, recaps off or AI off", false, true, false, 1},
		{"not kept (an instant meeting, a breakout room), recaps on", true, false, false, 0},
		{"not kept, recaps off", false, false, false, 0},
	} {
		asked := 0
		recapMade = func(context.Context) bool { asked++; return c.made }
		if got := makesSummary(ctx, &TranscriptionSwitch{On: true, Kept: c.kept}); got != c.want || asked != c.asked {
			t.Errorf("%s: summary %v (AI asked %d times), want %v (%d)", c.name, got, asked, c.want, c.asked)
		}
	}
}

// What every client in the call reads from the room, a guest as much as a
// member: the switch says whether the call's summary is made, from the flip,
// and again as soon as the workspace's answer changes while the call goes on.
// Only a room that keeps the transcript ever says yes; a breakout room of the
// same call, and a room nobody has switched, are left as they are.
func TestEveryoneInTheCallReadsWhetherItsSummaryIsMade(t *testing.T) {
	const room, breakout, quiet = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33", "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33-br-1", "5a1d3b2f-4c8e-4d66-8b62-1e9c2a7b3d44"
	lk := livekitfake.Serve(t, room, "RM_main", "0x1", "guest-0f3c")
	lk.Room(breakout, "RM_br", "0x2")
	lk.Room(quiet, "RM_quiet", "0x3")
	defer func(was func(context.Context, string) ([]string, error)) { callRooms = was }(callRooms)
	callRooms = func(_ context.Context, r string) ([]string, error) {
		if r == room || r == breakout {
			return []string{room, breakout}, nil
		}
		return []string{r}, nil
	}
	defer func(was func(string) string) { callOwnRoom = was }(callOwnRoom)
	callOwnRoom = func(r string) string {
		if r == breakout {
			return room
		}
		return r
	}
	defer func(was func(context.Context) bool) { recapMade = was }(recapMade)
	made := true
	recapMade = func(context.Context) bool { return made }
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	ctx := context.Background()

	says := func(r string) *TranscriptionSwitch {
		t.Helper()
		return parseRoomMeta(lk.Metadata(r)).Transcription
	}
	sw, err := SetTranscription(ctx, room, "0x1", "Maya", true)
	if err != nil {
		t.Fatal(err)
	}
	if !sw.Kept || !sw.Summary {
		t.Fatalf("the flipper is told %+v, want kept with a summary", sw)
	}
	if got := says(room); got == nil || !got.On || !got.Summary {
		t.Fatalf("the call's room says %+v, want on with a summary", got)
	}
	if got := says(breakout); got == nil || !got.On || got.Kept || got.Summary {
		t.Fatalf("its breakout room says %+v, want on, captions only, no summary", got)
	}

	// The admin turns recaps (or AI) off mid-call: told without a flip.
	made = false
	RefreshSummaries(ctx)
	if got := says(room); got == nil || !got.On || !got.Kept || got.Summary || got.By != "Maya" {
		t.Fatalf("after recaps went off the room says %+v, want Maya's switch, kept, no summary", got)
	}
	if got := says(quiet); got != nil {
		t.Fatalf("a room nobody switched was written: %+v", got)
	}
	// And on again.
	made = true
	RefreshSummaries(ctx)
	if got := says(room); got == nil || !got.Summary {
		t.Fatalf("after recaps came on the room says %+v, want a summary", got)
	}
	if got := says(breakout); got == nil || got.Summary {
		t.Fatalf("after recaps came on the breakout room says %+v, want no summary", got)
	}

	// Switched off, the room still says so of its kept lines, as it says kept.
	if sw, err = SetTranscription(ctx, room, "0x1", "Maya", false); err != nil || sw.On || !sw.Summary {
		t.Fatalf("switched off: %+v, %v", sw, err)
	}
}

// V-951 F1 (CR-271). Telling every call again asks the workspace once per run,
// however many calls keep a transcript: the setting is an uncached read.
func TestTellingEveryCallAgainAsksTheWorkspaceOncePerRun(t *testing.T) {
	const one, two = "6b2e4c3a-5d9f-4e77-9c73-2f0d3b8c4e55", "7c3f5d4b-6e0a-4f88-8d84-3a1e4c9d5f66"
	lk := livekitfake.Serve(t, one, "RM_one", "0x1")
	lk.Room(two, "RM_two", "0x2")
	defer func(was func(context.Context, string) ([]string, error)) { callRooms = was }(callRooms)
	callRooms = func(_ context.Context, r string) ([]string, error) { return []string{r}, nil }
	defer func(was func(string) string) { callOwnRoom = was }(callOwnRoom)
	callOwnRoom = func(r string) string { return r }
	defer func(was func(context.Context) bool) { recapMade = was }(recapMade)
	made, asked := true, 0
	recapMade = func(context.Context) bool { asked++; return made }
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	ctx := context.Background()

	for r, who := range map[string]string{one: "0x1", two: "0x2"} {
		if sw, err := SetTranscription(ctx, r, who, "Maya", true); err != nil || !sw.Kept || !sw.Summary {
			t.Fatalf("switching %s on: %+v, %v", r, sw, err)
		}
	}
	for _, c := range []struct {
		name string
		made bool
	}{{"recaps go off", false}, {"nothing changed", false}, {"recaps come on", true}} {
		made, asked = c.made, 0
		RefreshSummaries(ctx)
		if asked != 1 {
			t.Fatalf("%s: the workspace was asked %d times in one run, want once", c.name, asked)
		}
		for _, r := range []string{one, two} {
			if got := parseRoomMeta(lk.Metadata(r)).Transcription; got == nil || got.Summary != c.made {
				t.Fatalf("%s: %s says %+v, want summary %v", c.name, r, got, c.made)
			}
		}
	}
}

// V-951 F2 (CR-271). Recaps turned on, then off, inside one run's wait: the
// run that began first (it read "made") is slow on a room, and the newer run
// (it read "not made") has told that room already. The older run's answer
// never lands after the newer one's, or the call would promise a summary
// nobody writes.
func TestAnOlderRunNeverWritesOverANewerRunsAnswer(t *testing.T) {
	const room = "8d4a6e5c-7f1b-4a99-8e95-4b2f5d0e6a77"
	lk := livekitfake.Serve(t, room, "RM_main", "0x1")
	defer func(was func(context.Context, string) ([]string, error)) { callRooms = was }(callRooms)
	callRooms = func(_ context.Context, r string) ([]string, error) { return []string{r}, nil }
	defer func(was func(string) string) { callOwnRoom = was }(callOwnRoom)
	callOwnRoom = func(r string) string { return r }
	defer func(was func(context.Context) bool) { recapMade = was }(recapMade)
	recapMade = func(context.Context) bool { return true }
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	ctx := context.Background()
	if sw, err := SetTranscription(ctx, room, "0x1", "Maya", true); err != nil || !sw.Summary {
		t.Fatalf("switching on: %+v, %v", sw, err)
	}

	older := summaryRuns.Add(1) // began while recaps were on
	newer := summaryRuns.Add(1) // began after they went off
	if err := refreshSummary(ctx, room, newer, func() bool { return false }); err != nil {
		t.Fatal(err)
	}
	if got := parseRoomMeta(lk.Metadata(room)).Transcription; got == nil || got.Summary {
		t.Fatalf("the newer run left %+v, want no summary", got)
	}
	if err := refreshSummary(ctx, room, older, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got := parseRoomMeta(lk.Metadata(room)).Transcription; got == nil || got.Summary {
		t.Fatalf("the older run wrote its answer over the newer one's: %+v", got)
	}
}
