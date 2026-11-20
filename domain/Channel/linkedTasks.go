package domain

// The tasks a channel's messages link to (gcal-14 (d), R-CHANNEL): a channel's
// calendar shows them. A message's links are the work graph's refers_to
// edges (domain/WorkGraph refs.go), kept in step with its text, so this reads
// the edges and never a message's body.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// MaxLinkedTasks bounds the tasks one LinkedTasks read answers for a channel.
const MaxLinkedTasks = 2000

// LinkedTasks is, by channel (lower-case uuid), the live tasks that meet cond
// (a condition on tasks, such as a TaskListFilter's Clause; "" for every one)
// and that the live messages of these live channels link to, each once and at
// most MaxLinkedTasks a channel, in one read. A deleted message's links, a
// deleted task and a restricted task the reader of ctx may not see
// (models/taskread) are left out. Each task carries its name and its start
// and due dates.
func LinkedTasks(ctx context.Context, channelUUIDs []string, cond string) (map[string][]*dgraphStruct.DgraphTask, error) {
	out := make(map[string][]*dgraphStruct.DgraphTask, len(channelUUIDs))
	if len(channelUUIDs) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(channelUUIDs)
	if err != nil {
		return nil, err
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	filter := `has(task_uuid) AND ` + dgraphquery.Live("task_deleted_at")
	if strings.TrimSpace(cond) != "" {
		filter += ` AND (` + cond + `)`
	}
	query := tr.Fill(fmt.Sprintf(`{
		channel(func: eq(ch_uuid, %s)) @filter(%s) {
			ch_uuid
			ch_posts @filter(%s AND has(refers_to)) {
				refers_to @filter(%s %s) {
					task_uuid
					task_name
					task_start_date
					task_due_date
				}
			}
		}
	}`, ids, dgraphquery.Live("ch_deleted_at"), dgraphquery.Live("post_deleted_at"), filter, taskread.Mark))
	var res struct {
		Channel []struct {
			UUID  string `json:"ch_uuid"`
			Posts []struct {
				Tasks []*dgraphStruct.DgraphTask `json:"refers_to"`
			} `json:"ch_posts"`
		} `json:"channel"`
	}
	if err := dgraphInit.ReadInto(ctx, query, nil, &res); err != nil {
		return nil, err
	}
	for _, c := range res.Channel {
		key := strings.ToLower(c.UUID)
		seen := map[string]bool{}
		for _, p := range c.Posts {
			for _, t := range p.Tasks {
				if t == nil || t.Uuid == "" || seen[strings.ToLower(t.Uuid)] || len(out[key]) == MaxLinkedTasks {
					continue
				}
				seen[strings.ToLower(t.Uuid)] = true
				out[key] = append(out[key], t)
			}
		}
	}
	return out, nil
}
