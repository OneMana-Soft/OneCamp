package helpers

import "testing"

func TestDiskLevels(t *testing.T) {
	for pct, want := range map[int]string{0: "ok", 84: "ok", 85: "warn", 94: "warn", 95: "critical", 100: "critical"} {
		if got := (DiskStat{UsedPct: pct}).Level(); got != want {
			t.Errorf("%d%% is %q, want %q", pct, got, want)
		}
	}
}

func TestUsedPctRoundsUpLikeDf(t *testing.T) {
	if got := usedPct(851, 149); got != 86 {
		t.Errorf("85.1%% reads %d, want 86", got)
	}
	if got := usedPct(85, 15); got != 85 {
		t.Errorf("exactly 85%% reads %d", got)
	}
	if got := usedPct(0, 0); got != 0 {
		t.Errorf("an empty filesystem reads %d", got)
	}
}

func TestServerDiskReadsThisMachine(t *testing.T) {
	d, err := DiskUsage("/")
	if err != nil {
		t.Skip("no statfs here:", err)
	}
	if d.TotalBytes == 0 || d.UsedPct < 0 || d.UsedPct > 100 || d.FreeBytes > d.TotalBytes {
		t.Fatalf("implausible: %+v", d)
	}
}

func TestHumanBytes(t *testing.T) {
	for b, want := range map[uint64]string{640_000: "640 KB", 640_000_000: "640 MB", 12_400_000_000: "12.4 GB"} {
		if got := HumanBytes(b); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", b, got, want)
		}
	}
}
