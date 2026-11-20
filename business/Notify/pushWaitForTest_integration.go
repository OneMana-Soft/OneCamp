//go:build integration
// +build integration

package business

import (
	"context"
	"errors"
)

// SendWaitingPushesForTest is one pass of the Sweeper's notify.push.wait, for
// an integration test.
func SendWaitingPushesForTest(ctx context.Context) error { return sendWaitingPushes(ctx) }

// FailPayloadsForTest makes keeping a waiting push's payload fail, as Redis
// blinking would, until restore.
func FailPayloadsForTest() (restore func()) {
	was := keepPayloads
	keepPayloads = func(context.Context, []keptPayload) error { return errors.New("redis blinked (test)") }
	return func() { keepPayloads = was }
}
