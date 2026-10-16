package business

import (
	"context"
	"fmt"

	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// MentionRefusal is what to tell someone whose comment on the task mentions
// people who may not read it (CanViewTask), or "" when everyone it mentions
// may. A mention tells its person of the comment, by email too, so it names
// only someone the task is shown to: before, a mention of anyone in the
// workspace emailed them the comment and the task's name.
func MentionRefusal(ctx context.Context, taskUUID string, mentioned []*dgraphStruct.DgraphUser) (string, error) {
	ids := make([]string, 0, len(mentioned))
	for _, m := range mentioned {
		if m != nil {
			ids = append(ids, m.Uuid)
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	readers, err := domain.Readers(ctx, taskUUID, ids)
	if err != nil {
		return "", err
	}
	var outside []string
	for _, m := range mentioned {
		if m == nil || readers[m.Uuid] {
			continue
		}
		name := m.DisplayName()
		if name == "" {
			name = "Someone"
		}
		outside = append(outside, name)
	}
	return outsidersRefusal(outside), nil
}

// outsidersRefusal is the refusal of a comment that mentions these people,
// who can't see its task; "" for nobody. Pure.
func outsidersRefusal(names []string) string {
	if len(names) == 0 {
		return ""
	}
	who, mention := names[0]+" isn't", "mention"
	switch {
	case len(names) == 2:
		who, mention = names[0]+" and "+names[1]+" aren't", "mentions"
	case len(names) > 2:
		who, mention = fmt.Sprintf("%s and %d others aren't", names[0], len(names)-1), "mentions"
	}
	return who + " in this project, so they can't see this task. Take the " + mention + " out, or have them added to the project first."
}
