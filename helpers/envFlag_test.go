package helpers

import "testing"

func TestEnvFlag(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "True": true, "TRUE": true, " true ": true,
		"1": true, "yes": true, "Yes": true, "on": true, "ON": true,
		"": false, "false": false, "False": false, "0": false, "no": false,
		"off": false, "enabled": false, "ture": false, "2": false,
	} {
		t.Setenv("ONECAMP_TEST_FLAG", value)
		if got := EnvFlag("ONECAMP_TEST_FLAG"); got != want {
			t.Errorf("EnvFlag with %q = %v, want %v", value, got, want)
		}
	}
	if EnvFlag("ONECAMP_TEST_FLAG_NEVER_SET") {
		t.Error("an unset flag reads as on")
	}
}
