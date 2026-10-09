package business

import (
	"testing"
	"time"
)

// An explicit ONECAMP_MANAGED wins over the mark stored at Cloud's handover,
// false as much as true: a workspace taken off Cloud says false and is no
// longer treated as Cloud's. Unset, the mark decides.
func TestAnExplicitManagedSwitchWinsOverTheStoredMark(t *testing.T) {
	seed := func(values map[string]string) {
		settingsMu.Lock()
		settingsCache, settingsExpires = values, time.Now().Add(time.Minute)
		settingsMu.Unlock()
	}
	t.Cleanup(forget)
	marked := map[string]string{keyManagedByCloud: "true"}
	for _, c := range []struct {
		env  string
		mark map[string]string
		want bool
	}{
		{"false", marked, false},
		{"0", marked, false},
		{"true", map[string]string{}, true},
		{"", marked, true},
		{"", map[string]string{}, false},
		{"maybe", marked, true},
	} {
		t.Setenv("ONECAMP_MANAGED", c.env)
		seed(c.mark)
		if got := Managed(); got != c.want {
			t.Errorf("ONECAMP_MANAGED=%q with mark %v: managed %v, want %v", c.env, c.mark, got, c.want)
		}
	}
}
