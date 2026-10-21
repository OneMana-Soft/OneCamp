package business

// What an import's message text names (mrkdwn.go): the people and channels a
// message mentions, as this import brought them across, read before the
// message is rendered, a message's new names at a time, and kept for the
// chunk. A message's mentions cost at most four reads, however many it has:
// the people's mappings and their graph nodes, the channels' mappings and the
// names the import gave them.

import (
	"context"
	"encoding/json"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// textNames reads and keeps what a chunk's messages mention.
type textNames struct {
	importID uuid.UUID
	maps     *messageMaps
	names    mrkdwnNames
}

// Seams for tests: the people a mention names, by their uuids, and the
// channels' mappings and the names the import gave them.
var (
	readMentioned  = userDomain.GetActiveDgraphUsersByUUIDsLight
	lookupChannels = func(ctx context.Context, importID uuid.UUID, slackIDs []string) (map[string]uuid.UUID, error) {
		return importModels.LookupIdMappingsBatch(ctx, importID, importModels.EntityChannel, slackIDs)
	}
	channelMetadata = func(ctx context.Context, importID uuid.UUID, slackIDs []string) (map[string]json.RawMessage, error) {
		return importModels.IdMapMetadataMany(ctx, importID, importModels.EntityChannel, slackIDs)
	}
)

// newTextNames is a chunk's reader of what its messages mention, with the
// export's Slack names (slackNameBook) for those who didn't come across.
func newTextNames(importID uuid.UUID, maps *messageMaps, slackNames map[string]string) *textNames {
	return &textNames{importID: importID, maps: maps, names: mrkdwnNames{
		people:   map[string]mentionee{},
		slack:    slackNames,
		channels: map[string]channelChip{},
	}}
}

// of is what m's text names, reading those not known yet. A read that fails
// leaves its names unknown, so they are written as their Slack names: the
// message comes across either way.
func (n *textNames) of(ctx context.Context, m *SlackMessage) mrkdwnNames {
	users, channels := mentionedIDs(m.Text)
	if err := n.readPeople(ctx, users); err != nil {
		helpers.LogWarnWithContext(ctx, "SlackImport: the people message %s mentions weren't read: %+v", m.Ts, err)
	}
	if err := n.readChannels(ctx, channels); err != nil {
		helpers.LogWarnWithContext(ctx, "SlackImport: the channels message %s mentions weren't read: %+v", m.Ts, err)
	}
	return n.names
}

// readPeople reads the people of ids not known yet: their mappings, then the
// graph nodes of those who came across.
func (n *textNames) readPeople(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range ids {
		if _, known := n.names.people[id]; !known {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	mapped, err := n.maps.userIdsOf(ctx, missing)
	if err != nil {
		return err
	}
	var uuids []string
	for _, id := range missing {
		if u := mapped[id]; u != uuid.Nil {
			uuids = append(uuids, u.String())
		}
	}
	var people []*dgraphStruct.DgraphUser
	if len(uuids) > 0 {
		if people, err = readMentioned(ctx, uuids); err != nil {
			return err
		}
	}
	byUUID := make(map[uuid.UUID]*dgraphStruct.DgraphUser, len(people))
	for _, p := range people {
		if p == nil {
			continue
		}
		if u, err := uuid.Parse(p.Uuid); err == nil {
			byUUID[u] = p
		}
	}
	for _, id := range missing {
		who := mentionee{}
		if p := byUUID[mapped[id]]; p != nil && p.Uid != "" {
			who = mentionee{UUID: p.Uuid, UID: p.Uid, Name: p.DisplayName()}
		}
		n.names.people[id] = who
	}
	return nil
}

// readChannels reads the channels of ids not known yet: their mappings and
// the names the import gave them (its final_name).
func (n *textNames) readChannels(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range ids {
		if _, known := n.names.channels[id]; !known {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	mapped, err := lookupChannels(ctx, n.importID, missing)
	if err != nil {
		return err
	}
	meta, err := channelMetadata(ctx, n.importID, missing)
	if err != nil {
		return err
	}
	for _, id := range missing {
		chip := channelChip{}
		if u := mapped[id]; u != uuid.Nil {
			var m struct {
				FinalName string `json:"final_name"`
			}
			_ = json.Unmarshal(meta[id], &m)
			chip = channelChip{UUID: u.String(), Name: m.FinalName}
		}
		n.names.channels[id] = chip
	}
	return nil
}
