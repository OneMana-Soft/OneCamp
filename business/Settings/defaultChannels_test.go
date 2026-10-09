package business

import (
	"reflect"
	"testing"
)

// No choice and an empty choice are different answers: the first means
// #general, the second means an admin decided new members join nothing.
func TestDefaultChannelsTellNoChoiceFromAnEmptyOne(t *testing.T) {
	if ids, chosen := parseDefaultChannels("", false); chosen || ids != nil {
		t.Errorf("absent setting: got %v chosen=%v, want no choice", ids, chosen)
	}
	if ids, chosen := parseDefaultChannels("", true); !chosen || len(ids) != 0 {
		t.Errorf("saved empty: got %v chosen=%v, want an empty choice", ids, chosen)
	}
	ids, chosen := parseDefaultChannels(" B , a,, b ,A", true)
	if !chosen || !reflect.DeepEqual(ids, []string{"b", "a"}) {
		t.Errorf("got %v chosen=%v, want [b a] in the order chosen, without repeats", ids, chosen)
	}
}

// OneCamp Cloud says so in the environment once it has lent the workspace
// email; until then the mark its handover left says it (a database, which a
// unit test does not have, so only the environment is exercised here).
func TestManagedFollowsTheEnvironment(t *testing.T) {
	t.Setenv("ONECAMP_MANAGED", "true")
	if !Managed() {
		t.Error("ONECAMP_MANAGED=true is not a managed workspace")
	}
	t.Setenv("ONECAMP_MANAGED", "")
	if Managed() {
		t.Error("a workspace with no sign of Cloud reads as managed")
	}
}
