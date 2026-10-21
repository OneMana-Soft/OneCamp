package business

// A doc's pages hear of its changes on the events bus: a save, a rename, a new
// look, a trash, a restore or an earlier version brought back drops the kept
// copies of its published pages, so their next answer is rendered afresh. A
// copy is kept 30 seconds at most either way (registry.PublishPage), so a sink
// that failed leaves a page that late, never longer. A doc in the trash, or
// deleted for good, opens no page at all: its grants open nothing
// (business/Guest).

import (
	"context"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// pageKinds are the doc events that change what its pages show.
var pageKinds = map[resourceEventBusiness.Kind]bool{
	resourceEventBusiness.DocUpdated:        true,
	resourceEventBusiness.DocRenamed:        true,
	resourceEventBusiness.DocRestyled:       true,
	resourceEventBusiness.DocDeleted:        true,
	resourceEventBusiness.DocArchived:       true,
	resourceEventBusiness.DocRestored:       true,
	resourceEventBusiness.DocRecovered:      true,
	resourceEventBusiness.DocVersionRestore: true,
}

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:  "publish.pages",
		Wants: wantsPageEvent,
		Run:   onPageEvent,
	})
}

func wantsPageEvent(e resourceEventBusiness.Event) bool {
	return e.ResourceKind == "doc" && pageKinds[e.Kind]
}

func onPageEvent(ctx context.Context, e resourceEventBusiness.Event) {
	changed(ctx, e.ResourceUUID)
}
