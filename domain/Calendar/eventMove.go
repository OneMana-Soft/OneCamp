package Calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// graphUID is a node's uid as Dgraph writes it.
var graphUID = regexp.MustCompile(`^0x[0-9a-f]+$`)

// GetDgraphEventsPage is the next first Event nodes after the node uid after
// ("" from the start), in uid order: every event the graph holds, a page at a
// time, for the move into the calendar's one store. Each comes with its
// organiser's and guests' uuids.
func GetDgraphEventsPage(ctx context.Context, after string, first int) ([]*dgraphStruct.DgraphEvent, error) {
	from := ""
	if after != "" {
		if !graphUID.MatchString(after) {
			return nil, fmt.Errorf("domain/GetDgraphEventsPage: %q isn't a uid", after)
		}
		from = ", after: " + after
	}
	query := fmt.Sprintf(`{
		events(func: has(event_uuid), first: %d%s) {
			uid
			event_uuid
			event_title
			event_description
			event_start_time
			event_end_time
			event_google_calendar_id
			event_is_focus
			event_is_away
			event_created_at
			event_updated_at
			event_created_by { user_uuid }
			event_participants { user_uuid }
		}
	}`, first, from)
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphEventsPage err: %+v", err)
		return nil, err
	}
	var out struct {
		Events []*dgraphStruct.DgraphEvent `json:"events"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}
