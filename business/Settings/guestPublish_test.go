package business

import (
	"testing"

	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// Publishing pages is guest access's one sub-option: on unless an admin turns
// it off, and nothing at all while guest access is off, so one switch stops
// links and pages alike.
func TestPublishingIsASubOptionOfGuestAccess(t *testing.T) {
	prev := readSettings
	t.Cleanup(func() { readSettings = prev; forget() })
	t.Setenv("GUEST_ACCESS_ENABLED", "")
	rows := map[string]string{}
	readSettings = func() ([]*configModel.SystemConfig, error) {
		var out []*configModel.SystemConfig
		for k, v := range rows {
			out = append(out, &configModel.SystemConfig{Key: k, Value: v})
		}
		return out, nil
	}
	for _, c := range []struct {
		guest, publish string
		allowed, on    bool
	}{
		{"", "", true, false},           // guest access is off until an admin turns it on
		{"true", "", true, true},        // the sub-option is on unless turned off
		{"true", "false", false, false}, // turned off: no pages, links still work
		{"false", "true", true, false},  // guest access off stops pages too
		{"true", "true", true, true},
	} {
		forget()
		rows = map[string]string{}
		if c.guest != "" {
			rows[keyGuestAccess] = c.guest
		}
		if c.publish != "" {
			rows[keyGuestPublish] = c.publish
		}
		if GuestPublishAllowed() != c.allowed || GuestPublishEnabled() != c.on {
			t.Errorf("guest %q, publish %q: allowed %v, on %v", c.guest, c.publish, GuestPublishAllowed(), GuestPublishEnabled())
		}
		if GetStatus().GuestPublishAllowed != c.allowed {
			t.Errorf("guest %q, publish %q: the admin view says %v", c.guest, c.publish, GetStatus().GuestPublishAllowed)
		}
	}
}

// Personal meeting rooms are guest access's second sub-option, on the same
// terms: on unless turned off, and nothing while guest access is off, so
// either switch stops every room at once.
func TestRoomsAreASubOptionOfGuestAccess(t *testing.T) {
	prev := readSettings
	t.Cleanup(func() { readSettings = prev; forget() })
	t.Setenv("GUEST_ACCESS_ENABLED", "")
	rows := map[string]string{}
	readSettings = func() ([]*configModel.SystemConfig, error) {
		var out []*configModel.SystemConfig
		for k, v := range rows {
			out = append(out, &configModel.SystemConfig{Key: k, Value: v})
		}
		return out, nil
	}
	for _, c := range []struct {
		guest, rooms, publish string
		allowed, on, pages    bool
	}{
		{"", "", "", true, false, false},
		{"true", "", "", true, true, true},
		{"true", "false", "", false, false, true},     // rooms off: pages carry on
		{"true", "", "false", true, true, false},      // pages off: rooms carry on
		{"false", "true", "true", true, false, false}, // guest access off stops both
	} {
		forget()
		rows = map[string]string{}
		for k, v := range map[string]string{keyGuestAccess: c.guest, keyGuestRooms: c.rooms, keyGuestPublish: c.publish} {
			if v != "" {
				rows[k] = v
			}
		}
		if GuestRoomsAllowed() != c.allowed || GuestRoomsEnabled() != c.on || GuestPublishEnabled() != c.pages {
			t.Errorf("guest %q rooms %q publish %q: allowed %v, rooms %v, pages %v", c.guest, c.rooms, c.publish, GuestRoomsAllowed(), GuestRoomsEnabled(), GuestPublishEnabled())
		}
		if GetStatus().GuestRoomsAllowed != c.allowed {
			t.Errorf("guest %q rooms %q: the admin view says %v", c.guest, c.rooms, GetStatus().GuestRoomsAllowed)
		}
	}
}
