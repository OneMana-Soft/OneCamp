package business

import (
	"context"
	"testing"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
)

// RELEASE-LINE ONLY (v2.75.0 / v1.60.0). The sweep's Inbox notice sends admins
// to Settings > Docs to hand a departed person's work on, and this release's
// web has no such page (docs C6-2's web isn't in it). So the release lines
// don't register the sweep; beta does, and the release that carries C6-2's web
// drops this test and registers it again. Registering the same name twice
// panics (business/Sweep), so a registration here that doesn't panic proves
// the sweep isn't registered.
func TestTheReleaseLineDoesNotRunTheDepartedSweep(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the departed sweep is registered on the release line (%v), but this web has no Settings > Docs for its notice", r)
		}
	}()
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: SweepName, Every: time.Hour, Run: func(context.Context) error { return nil }})
}
