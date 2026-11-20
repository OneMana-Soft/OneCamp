// Package domain (NotificationDelivery) is the query text of the
// Notification Router's record of what it told whom (migrations 585 and
// 592): business/Notify owns its rules, and models/postgres/NotificationDelivery
// runs the text through models/pgexec.
package domain

import (
	"fmt"
	"strings"
)

// RecordParams is how many parameters each row of Record takes.
const RecordParams = 9

// Record writes rows rows, each (event, person, delivery) once: one already
// written is left as it is. Each row's parameters, in order: the event, the
// person, the delivery, its cause, its state, the message ("" for none), when
// it's held until, when it was sent, and when it was written: NULL for now
// (the column's own default), or the time of a message written earlier (an
// import, the demo's seed), so its deliveries never read later than it. It
// answers the rows it wrote, by lower-case user id and delivery.
func Record(rows int) string {
	values := make([]string, rows)
	for i := range values {
		n := i * RecordParams
		values[i] = fmt.Sprintf("($%d, $%d::uuid, $%d, $%d, $%d, NULLIF($%d, '')::uuid, $%d::timestamptz, $%d::timestamptz, COALESCE($%d::timestamptz, NOW()))",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9)
	}
	return `INSERT INTO notification_deliveries (event_id, user_id, delivery, cause, state, message_id, hold_until, sent_at, created_at)
		VALUES ` + strings.Join(values, ", ") + `
		ON CONFLICT (event_id, user_id, delivery) DO NOTHING
		RETURNING lower(user_id::text), delivery`
}
