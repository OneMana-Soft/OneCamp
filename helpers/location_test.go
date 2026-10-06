package helpers

import (
	"testing"
	"time"
)

func TestLocation(t *testing.T) {
	if Location("Asia/Kolkata").String() != "Asia/Kolkata" {
		t.Error("a known zone")
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus", "../../etc/passwd"} {
		if Location(bad) != time.UTC {
			t.Errorf("%q must fall back to UTC", bad)
		}
	}
}
