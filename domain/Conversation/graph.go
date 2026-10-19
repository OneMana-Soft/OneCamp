package domain

// What the graph says about conversations: the side the index is filled in from
// and compared with (business/Conversation backfill.go and reconcile.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// parsePeopleUUIDs reads a "people" answer's user_uuids. Pure.
func parsePeopleUUIDs(raw []byte) ([]string, error) {
	var parsed struct {
		People []struct {
			UUID string `json:"user_uuid"`
		} `json:"people"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.People))
	for _, p := range parsed.People {
		if p.UUID != "" {
			out = append(out, p.UUID)
		}
	}
	return out, nil
}

// GraphConversation is one conversation as the graph holds it: its people,
// and for a channel its managers among them.
type GraphConversation struct {
	UID       string
	Kind      string
	Ref       string
	CreatedBy string
	CreatedAt time.Time
	Members   []string
	Managers  []string
}

// GraphPage is a page of the graph's conversations of one kind (channels, or
// DMs and groups), after the uid after ("" for the first), and the uid to ask
// after next ("" when it was the last page).
func GraphPage(ctx context.Context, channels bool, after string, size int) ([]GraphConversation, string, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, "", errors.New("no graph connection")
	}
	paging := fmt.Sprintf("first: %d", size)
	if after != "" {
		paging += ", after: " + after
	}
	var q string
	if channels {
		q = `{ page(func: type(Channel), ` + paging + `) {
			uid ch_uuid ch_created_at
			ch_created_by { user_uuid }
			ch_members { user_uuid }
			ch_moderators { user_uuid }
		} }`
	} else {
		q = `{ page(func: type(Dm), ` + paging + `) {
			uid dm_grouping_id
			dm_participants { user_uuid }
		} }`
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, q)
	if err != nil {
		return nil, "", err
	}
	out, err := parseGraphPage(resp.Json, channels)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) == size {
		next = out[len(out)-1].UID
	}
	// A node with no id of its own (half-made, or left by an old bug) has
	// nothing to be indexed under.
	kept := out[:0]
	for _, c := range out {
		if c.Ref != "" {
			kept = append(kept, c)
		}
	}
	return kept, next, nil
}

type person struct {
	UUID string `json:"user_uuid"`
}

func uuidsOf(ps []person) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.UUID != "" {
			out = append(out, p.UUID)
		}
	}
	return out
}

// parseGraphPage reads a page. A DM's grouping id has a space, a group's
// hasn't. Pure.
func parseGraphPage(raw []byte, channels bool) ([]GraphConversation, error) {
	var parsed struct {
		Page []struct {
			UID        string     `json:"uid"`
			Channel    string     `json:"ch_uuid"`
			CreatedAt  *time.Time `json:"ch_created_at"`
			CreatedBy  *person    `json:"ch_created_by"`
			Members    []person   `json:"ch_members"`
			Moderators []person   `json:"ch_moderators"`
			Grouping   string     `json:"dm_grouping_id"`
			People     []person   `json:"dm_participants"`
		} `json:"page"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := make([]GraphConversation, 0, len(parsed.Page))
	for _, n := range parsed.Page {
		c := GraphConversation{UID: n.UID}
		if channels {
			c.Kind, c.Ref = KindChannel, n.Channel
			c.Members, c.Managers = uuidsOf(n.Members), uuidsOf(n.Moderators)
			if n.CreatedBy != nil {
				c.CreatedBy = n.CreatedBy.UUID
			}
			if n.CreatedAt != nil {
				c.CreatedAt = *n.CreatedAt
			}
		} else {
			c.Ref, c.Members = n.Grouping, uuidsOf(n.People)
			c.Kind = KindGroup
			if strings.Contains(n.Grouping, " ") {
				c.Kind = KindDM
			}
		}
		out = append(out, c)
	}
	return out, nil
}
