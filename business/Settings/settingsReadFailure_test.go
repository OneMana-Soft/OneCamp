package business

import (
	"errors"
	"testing"
	"time"

	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// A failed read of the settings is not "nothing set". It used to be cached as
// an empty map for the whole TTL, so a Postgres hiccup turned read receipts a
// workspace had switched off back on (and mail, guest links and the channels
// new members join back to their defaults) for half a minute. The last
// settings read are served instead, and the failure is not cached.
func TestAFailedReadKeepsTheLastSettings(t *testing.T) {
	prev := readSettings
	t.Cleanup(func() { readSettings = prev; forget() })
	forget()

	value, fail := "false", false
	readSettings = func() ([]*configModel.SystemConfig, error) {
		if fail {
			return nil, errors.New("connection refused")
		}
		return []*configModel.SystemConfig{{Key: keyReadReceipts, Value: value}}, nil
	}
	expire := func() {
		settingsMu.Lock()
		settingsExpires = time.Time{}
		settingsMu.Unlock()
	}

	if got := loadAll()[keyReadReceipts]; got != "false" {
		t.Fatalf("first read: %q, want false", got)
	}

	// The cache runs out, and the database doesn't answer.
	expire()
	fail = true
	if got := loadAll()[keyReadReceipts]; got != "false" {
		t.Fatalf("a failed read served %q; want the last setting read, false", got)
	}

	// The failure wasn't cached: the first read after the database is back is fresh.
	fail, value = false, "true"
	if got := loadAll()[keyReadReceipts]; got != "true" {
		t.Fatalf("after recovery %q, want true: a failed read must not be cached", got)
	}

	// With nothing read yet, a failure is an empty answer, still not cached.
	forget()
	fail = true
	if got := loadAll(); len(got) != 0 {
		t.Fatalf("nothing read yet: %v, want empty", got)
	}
	fail = false
	if got := loadAll()[keyReadReceipts]; got != "true" {
		t.Fatalf("the empty answer was cached: %q", got)
	}
}

// Saving a setting makes the next read go to the database, but it doesn't
// throw away what was read: a save followed by a read that fails still serves
// the workspace's settings, not their defaults. Dropping the cache on every
// save turned guest links off, until a read succeeded, whenever Postgres
// stumbled just after an admin saved anything.
func TestAFailedReadAfterASaveKeepsTheLastSettings(t *testing.T) {
	prev := readSettings
	t.Cleanup(func() { readSettings = prev; forget() })
	forget()

	fail := false
	readSettings = func() ([]*configModel.SystemConfig, error) {
		if fail {
			return nil, errors.New("connection refused")
		}
		return []*configModel.SystemConfig{{Key: keyReadReceipts, Value: "false"}}, nil
	}
	if got := loadAll()[keyReadReceipts]; got != "false" {
		t.Fatalf("first read: %q, want false", got)
	}

	invalidate() // what every save does
	fail = true
	if got := loadAll()[keyReadReceipts]; got != "false" {
		t.Fatalf("a failed read after a save served %q; want the last setting read, false", got)
	}
}
