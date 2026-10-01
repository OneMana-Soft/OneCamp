package business

import (
	"strings"
	"testing"
)

func TestRepeatedCallObservationGivesTheResultBack(t *testing.T) {
	obs := repeatedCallObservation("- Doc: Launch sync notes [doc_uuid=d-1]")
	if !strings.Contains(obs, "[doc_uuid=d-1]") {
		t.Fatalf("the earlier result must come back: %q", obs)
	}
	if !strings.Contains(obs, "Do not repeat the call") {
		t.Fatalf("the model must be told to move on: %q", obs)
	}
}
