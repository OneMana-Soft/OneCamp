package business

import (
	"context"
	"testing"
)

// CALL-BUG-16. egress_ended without its egress is ignored, never
// dereferenced.
func TestARecordingEndWithoutItsEgressIsIgnored(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("egress_ended without an egress panicked: %v", r)
		}
	}()
	OnRecordingEnded(context.Background(), nil)
}
