package business

import (
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// The bus's sinks that live here, and the inbox's notices. Each runs for the
// events it wants, on its own (business/ResourceEvent sinks.go). The audit and
// a purge's cleanup run for a bulk import's events too; telling people, open
// tabs and webhooks don't.
func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "notify", Wants: notifies, Run: notify})
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "audit", Wants: isAudited, Run: record, InBulk: true})
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "doc.meta", Wants: isMeta, Run: publishDocMeta})
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "webhooks", Wants: isWebhook, Run: sendWebhook})
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "purge.forget", Wants: isPurge, Run: forgetPurged, InBulk: true})
	activityBusiness.NoticeSource = feedNotices
	lastSeenActivityBusiness.UnreadNotices = unreadNotices
}
