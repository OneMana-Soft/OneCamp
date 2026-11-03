package controllers

import (
	"fmt"
	"strings"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	"github.com/google/uuid"
)

// TestAProviderEditNeverRecordsItsKey: a new API key is recorded as changed,
// never its value, and a new address without a key in its query.
func TestAProviderEditNeverRecordsItsKey(t *testing.T) {
	key, label, on := "sk-live-do-not-record", "Own", true
	base := "https://models.example.test/v1?key=" + key
	meta := providerUpdateMeta(uuid.New(), adapter.UpdateProviderRequest{APIKey: &key, Label: &label, Enabled: &on, BaseURL: &base})
	if meta["api_key_changed"] != true || meta["label"] != "Own" || meta["enabled"] != true || meta["base_url"] != "https://models.example.test/v1" {
		t.Fatalf("meta %v: want the key marked changed, the label, the switch and the address without its query", meta)
	}
	if strings.Contains(fmt.Sprint(meta), key) {
		t.Fatalf("meta %v holds the API key", meta)
	}
	if m := providerUpdateMeta(uuid.New(), adapter.UpdateProviderRequest{}); m["api_key_changed"] != false || len(m) != 2 {
		t.Fatalf("an edit that sets nothing: %v", m)
	}
}

// TestARunnerSettingNeverRecordsItsToken: both runner routes (the sandbox and
// code pull requests) record the runner's address without the token its
// credentials or query carry, and record a new or cleared token as changed,
// never its value.
func TestARunnerSettingNeverRecordsItsToken(t *testing.T) {
	const token = "sk-runner-do-not-record"
	meta := runnerMeta(true, "https://tok:"+token+"@runner/?key="+token, token, false)
	if meta["enabled"] != true || meta["runner_url"] != "https://runner/" || meta["runner_token_changed"] != true || len(meta) != 3 {
		t.Fatalf("meta %v: want the switch, the address without its credentials or query, and the token marked changed", meta)
	}
	if strings.Contains(fmt.Sprint(meta), token) || strings.Contains(fmt.Sprint(meta), "tok:") {
		t.Fatalf("meta %v holds the runner's token", meta)
	}
	if m := runnerMeta(false, "", "", true); m["enabled"] != false || m["runner_url"] != "" || m["runner_token_changed"] != true {
		t.Fatalf("a cleared token on no runner: %v", m)
	}
	if m := runnerMeta(true, "https://runner.example.test/run", "  ", false); m["runner_url"] != "https://runner.example.test/run" || m["runner_token_changed"] != false {
		t.Fatalf("a blank token keeps the one stored: %v", m)
	}
}
