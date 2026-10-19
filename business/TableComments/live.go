package business

// Open tabs hear of a row's threads on the table's live topic: a thread made,
// replied in, resolved, reopened or moved, and a comment edited, removed or
// reacted to, each as {"action": "comments", "row_id", "thread_id",
// "comment_id", "event"}. Ids only, as every table message is: a tab that
// shows the row reads its threads again, as its reader may see them, and the
// grid reads the row's comment count with the row. Only people who may open
// the table hear its topic (business/MqttAccess).

import (
	"context"
	"strings"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// liveAction is the action a row's thread change has on its table's topic.
const liveAction = "comments"

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "table_row.live", Wants: isRowThreadEvent, Run: publishLive})
}

// Seams for tests: where a row's table is read, and where a message goes.
var (
	liveHomes   = model.RowHomes
	livePublish = mqttBusiness.PublishTableRow
)

// isRowThreadEvent is a thread event on a table's row. Pure.
func isRowThreadEvent(e resourceEventBusiness.Event) bool {
	return e.ResourceKind == Kind && strings.HasPrefix(string(e.Kind), "thread.")
}

func publishLive(ctx context.Context, e resourceEventBusiness.Event) {
	row, err := uuid.Parse(e.ResourceUUID)
	if err != nil {
		return
	}
	homes, err := liveHomes(ctx, []uuid.UUID{row})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TableComments: row %s's table couldn't be read, so its open tabs weren't told of %s: %+v", row, e.Kind, err)
		return
	}
	h, ok := homes[row]
	if !ok {
		return // the row is gone
	}
	livePublish(h.Table.String(), liveMessage(e))
}

// liveMessage is what the table's topic hears of a thread event: ids only,
// never a comment's words. Pure.
func liveMessage(e resourceEventBusiness.Event) map[string]interface{} {
	m := map[string]interface{}{"action": liveAction, "row_id": e.ResourceUUID, "event": string(e.Kind)}
	if id := e.Data["thread_id"]; id != "" {
		m["thread_id"] = id
	}
	if id := e.Data["comment_id"]; id != "" {
		m["comment_id"] = id
	}
	return m
}
