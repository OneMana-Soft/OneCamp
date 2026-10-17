package business

// What a doc leaves behind here goes when it does: a purge removes its follows,
// its notices and its mentions (a notice about a doc nobody can open again is
// only noise, and its id names nothing). Notices go after noticeRetention in any
// case, a batch at a time, by a claimed sweep.

import (
	"context"
	"sort"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
)

const (
	// noticeRetention is how long an inbox notice is kept.
	noticeRetention = 90 * 24 * time.Hour
	// noticeSweepBatch and noticeSweepPasses bound one run of the sweep.
	noticeSweepBatch  = 5000
	noticeSweepPasses = 20
)

// Seams for tests.
var (
	forgetResources = followModels.ForgetResources
	deleteNotices   = followModels.DeleteNoticesBefore
)

func isPurge(e resourceEventBusiness.Event) bool {
	return e.Kind == resourceEventBusiness.DocPurged && e.ResourceKind == "doc"
}

func forgetPurged(ctx context.Context, e resourceEventBusiness.Event) {
	if err := forgetResources(ctx, e.ResourceKind, []string{e.ResourceUUID}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Follow: doc %s's follows and notices weren't removed with it: %+v", e.ResourceUUID, err)
	}
}

// sweepNotices removes notices older than noticeRetention.
func sweepNotices(ctx context.Context) error {
	before := clock().Add(-noticeRetention)
	for i := 0; i < noticeSweepPasses; i++ {
		n, err := deleteNotices(ctx, before, noticeSweepBatch)
		if err != nil || n < noticeSweepBatch {
			return err
		}
	}
	return nil
}

// sortedFollowers is everyone in follows who chose mode, in a stable order.
// Pure.
func sortedFollowers(follows map[string]string, mode string) []string {
	var out []string
	for id, m := range follows {
		if m == mode {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "notices.retention", Every: 6 * time.Hour, Run: sweepNotices})
}
