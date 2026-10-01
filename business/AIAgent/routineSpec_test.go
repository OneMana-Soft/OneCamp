package business

import "testing"

func TestNormalizeRoutineInput(t *testing.T) {
	// Happy path: valid daily routine, explicit name kept, rule upper-cased.
	got, err := NormalizeRoutineInput(RoutineInput{
		Name:        "Morning digest",
		Prompt:      "summarize this channel",
		Recurrence:  "freq=daily",
		AtMinuteUTC: 540,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "Morning digest" || got.Prompt != "summarize this channel" {
		t.Fatalf("name/prompt mangled: %+v", got)
	}
	if got.Recurrence != "FREQ=DAILY" {
		t.Fatalf("recurrence not upper-cased: %q", got.Recurrence)
	}
	if got.AtMinuteUTC != 540 {
		t.Fatalf("minute mangled: %d", got.AtMinuteUTC)
	}

	// Blank prompt is rejected.
	if _, err := NormalizeRoutineInput(RoutineInput{Prompt: "  ", Recurrence: "FREQ=DAILY"}); err == nil {
		t.Fatalf("blank prompt should error")
	}

	// Missing/blank cadence is rejected.
	if _, err := NormalizeRoutineInput(RoutineInput{Prompt: "do x", Recurrence: " "}); err == nil {
		t.Fatalf("blank recurrence should error")
	}

	// Unfireable cadence (MONTHLY/YEARLY aren't supported).
	for _, bad := range []string{"FREQ=MONTHLY", "FREQ=YEARLY", "not-a-rule"} {
		if _, err := NormalizeRoutineInput(RoutineInput{Prompt: "do x", Recurrence: bad}); err == nil {
			t.Fatalf("cadence %q should be rejected", bad)
		}
	}

	// Weekly with BYDAY and hourly-interval cadences are accepted.
	for _, good := range []string{"FREQ=WEEKLY;BYDAY=MO,WE,FR", "FREQ=HOURLY", "FREQ=HOURLY;INTERVAL=6"} {
		if _, err := NormalizeRoutineInput(RoutineInput{Prompt: "do x", Recurrence: good}); err != nil {
			t.Fatalf("cadence %q should be valid: %v", good, err)
		}
	}

	// Name defaults to a short slug of the prompt when omitted.
	d, err := NormalizeRoutineInput(RoutineInput{
		Prompt:     "post the open pull requests and anything waiting on someone please",
		Recurrence: "FREQ=DAILY",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Name == "" || len(d.Name) > routineMaxName {
		t.Fatalf("derived name out of range: %q", d.Name)
	}
}

func TestWrapMinuteAndLocalConversion(t *testing.T) {
	// wrapMinute folds any value into [0,1439].
	cases := map[int]int{0: 0, 540: 540, 1439: 1439, 1440: 0, 1500: 60, -60: 1380, -1440: 0}
	for in, want := range cases {
		if got := wrapMinute(in); got != want {
			t.Fatalf("wrapMinute(%d) = %d, want %d", in, got, want)
		}
	}

	// 9:00am US Pacific (UTC-7 during DST → -420) is 16:00 UTC = 960.
	if got := MinuteUTCFromLocal(9, 0, -420); got != 960 {
		t.Fatalf("9am PDT should be 960 UTC minutes, got %d", got)
	}
	// 11:00pm at UTC+2 (+120) is 21:00 UTC = 1260.
	if got := MinuteUTCFromLocal(23, 0, 120); got != 1260 {
		t.Fatalf("11pm at +2 should be 1260, got %d", got)
	}
	// 1:00am at UTC+3 (+180) crosses midnight backward → 22:00 prev day = 1320.
	if got := MinuteUTCFromLocal(1, 0, 180); got != 1320 {
		t.Fatalf("1am at +3 should wrap to 1320, got %d", got)
	}
}
