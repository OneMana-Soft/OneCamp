package notification

// A VIP's words reach through the gate (chat P3 stage 3, TN-58).
//
// A person may name VIPs (models/postgres/PersonalAlert): people whose
// messages and mentions reach them through a pause, focus time and quiet
// hours, as Notify anyway does. Everything else about who is told stays the
// site's: the conversation's members, their level for it (a muted
// conversation stays muted), never the writer, never someone deactivated.

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	alertModels "github.com/akashc777/OneCamp/models/postgres/PersonalAlert"
)

// Seams for tests.
var (
	whoseVIP       = alertModels.WhoseVIP
	deliverNow     = Deliver
	deliverThrough = DeliverThroughPause
)

// DeliverFrom is Deliver for words sender (a user id) wrote: those of
// recipients who have sender as a VIP are told through their pause, focus
// time and quiet hours; the rest as Deliver tells them. If who has whom as a
// VIP can't be read, everyone is told as Deliver tells them.
func DeliverFrom(ctx context.Context, sender string, recipients []string, pushData map[string]string, noticeID string) {
	deliverFrom(ctx, sender, recipients, pushData, noticeID, deliverNow)
}

// deliverFrom is DeliverFrom telling the rest by now.
func deliverFrom(ctx context.Context, sender string, recipients []string, pushData map[string]string, noticeID string,
	now func(context.Context, []string, map[string]string, string)) {
	if sender == "" {
		now(ctx, recipients, pushData, noticeID)
		return
	}
	vip, err := whoseVIP(ctx, sender, recipients)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notification DeliverFrom %s: could not read VIPs: %+v", noticeID, err)
		vip = nil
	}
	through, rest := splitByVIP(recipients, vip)
	if len(rest) > 0 {
		now(ctx, rest, pushData, noticeID)
	}
	if len(through) > 0 {
		// A copy: the push writes into its data.
		data := make(map[string]string, len(pushData))
		for k, v := range pushData {
			data[k] = v
		}
		if err := deliverThrough(ctx, through, data, noticeID); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Notification DeliverFrom %s: %+v", noticeID, err)
		}
	}
}

// splitByVIP is recipients split into those vip names (by lower-cased id)
// and the rest, in order. Pure.
func splitByVIP(recipients []string, vip map[string]bool) (through, rest []string) {
	for _, r := range recipients {
		if vip[strings.ToLower(strings.TrimSpace(r))] {
			through = append(through, r)
		} else {
			rest = append(rest, r)
		}
	}
	return through, rest
}
