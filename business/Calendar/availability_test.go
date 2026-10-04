package Calendar

import (
	"testing"
	"time"
)

func TestMergeBusy(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 5, h, 0, 0, 0, time.UTC) }
	got := MergeBusy([]Interval{{at(13), at(14)}, {at(9), at(10)}, {at(10), at(11)}, {at(12), at(12)}, {at(13), at(15)}})
	want := []Interval{{at(9), at(11)}, {at(13), at(15)}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFreeSlots(t *testing.T) {
	kol, _ := time.LoadLocation("Asia/Kolkata")
	local := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, kol) }
	hours := WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "12:00", TZ: "Asia/Kolkata"}
	// Mon 5 Oct; busy 10:00-10:30.
	busy := []Interval{{local(5, 10, 0), local(5, 10, 30)}}
	starts := func(s []Interval) []string {
		var out []string
		for _, x := range s {
			out = append(out, x.Start.In(kol).Format("Mon 15:04"))
		}
		return out
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}

	day := FreeSlots(busy, local(5, 0, 0), local(6, 0, 0), hours, SlotRules{Duration: 30 * time.Minute})
	eq("plain", starts(day), []string{"Mon 09:00", "Mon 09:30", "Mon 10:30", "Mon 11:00", "Mon 11:30"})

	buffered := FreeSlots(busy, local(5, 0, 0), local(6, 0, 0), hours, SlotRules{Duration: 30 * time.Minute, Buffer: 15 * time.Minute})
	eq("buffer", starts(buffered), []string{"Mon 09:00", "Mon 11:00", "Mon 11:30"})

	notice := FreeSlots(busy, local(5, 0, 0), local(6, 0, 0), hours, SlotRules{Duration: 30 * time.Minute, Earliest: local(5, 10, 45)})
	eq("notice", starts(notice), []string{"Mon 11:00", "Mon 11:30"})

	// Saturday and Sunday are skipped; two a day at most.
	week := FreeSlots(nil, local(9, 0, 0), local(13, 0, 0), hours, SlotRules{Duration: time.Hour, PerDay: 2})
	eq("weekend and per-day", starts(week), []string{"Fri 09:00", "Fri 09:30", "Mon 09:00", "Mon 09:30"})

	if s := FreeSlots(nil, local(5, 0, 0), local(6, 0, 0), WorkingHours{Days: []int{1}, Start: "12:00", End: "09:00", TZ: "Asia/Kolkata"}, SlotRules{Duration: time.Hour}); s != nil {
		t.Fatalf("backwards hours gave %v", s)
	}
}

func TestFreeSlotsAcrossDaylightSaving(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	hours := WorkingHours{Days: []int{0, 1}, Start: "09:00", End: "10:00", TZ: "America/New_York"}
	// Clocks go back on Sun 1 Nov 2026; both days still offer 09:00 local.
	from := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	slots := FreeSlots(nil, from, from.Add(72*time.Hour), hours, SlotRules{Duration: time.Hour})
	if len(slots) != 2 {
		t.Fatalf("got %v", slots)
	}
	for _, s := range slots {
		if s.Start.In(ny).Hour() != 9 {
			t.Fatalf("slot at %v", s.Start.In(ny))
		}
	}
}

func TestWorkingHoursCheck(t *testing.T) {
	bad := []WorkingHours{
		{Days: nil, Start: "09:00", End: "17:00", TZ: "UTC"},
		{Days: []int{7}, Start: "09:00", End: "17:00", TZ: "UTC"},
		{Days: []int{1}, Start: "9:00", End: "17:00", TZ: "UTC"},
		{Days: []int{1}, Start: "09:00", End: "24:30", TZ: "UTC"},
		{Days: []int{1}, Start: "09:00", End: "17:00", TZ: "Mars/Base"},
	}
	for _, h := range bad {
		if _, err := h.Check(); err == nil {
			t.Errorf("accepted %+v", h)
		}
	}
	if _, err := (WorkingHours{Days: []int{1}, Start: "00:00", End: "24:00", TZ: "UTC"}).Check(); err != nil {
		t.Errorf("whole day: %v", err)
	}
}
