package models

import "testing"

// The app numbers these the same way, by position (onecamp-fe
// services/mqttService.ts, pinned by mqttService.enum.test.ts). Inserting a
// type anywhere but the end shifts every one after it, and the app then reads
// each message as something else.
func TestMessageTypesKeepTheirNumbers(t *testing.T) {
	if MESSAGE_AI_AGENT_WORK != 28 || MESSAGE_SAVED_ITEM_DUE != 29 {
		t.Fatalf("AI_AGENT_WORK=%d SAVED_ITEM_DUE=%d, want 28 and 29; append new types at the end", MESSAGE_AI_AGENT_WORK, MESSAGE_SAVED_ITEM_DUE)
	}
}
