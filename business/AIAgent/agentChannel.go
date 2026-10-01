package business

// In-channel "AI teammates": adding/removing an agent to a channel from inside
// that channel (the Slack/Claude-Tag "add the app to a channel" model),
// expressed through the agent's channel scope.
//
// An agent that is "in" a channel has that channel in its scope.channel_ids, so
// the mention trigger answers there (and only there, once any channel is set —
// the "invited or silent" gate in agentTriggers.go). This reuses the same scope
// the Agent Builder edits, so the in-channel control and the builder's channel
// picker stay one source of truth. Only mention-trigger agents are offered,
// since they are the ones that respond to an @mention in a channel.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// ChannelAgentOption is one selectable AI teammate for a channel: its identity
// plus whether it is currently active in this channel.
type ChannelAgentOption struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AvatarKey string `json:"avatar_key,omitempty"`
	InChannel bool   `json:"in_channel"`
	// Global is true when the agent has no channel scope at all, so it already
	// answers everywhere (adding it to a channel would, by design, narrow it to
	// only the channels you pick). Surfaced so the UI can explain this.
	Global bool `json:"global"`
}

// ListChannelAgentOptions returns the workspace's active mention-trigger agents
// and whether each is currently scoped to the given channel. Powers the
// in-channel "AI teammates" picker. Read-only; channel authorization is done by
// the controller.
func ListChannelAgentOptions(ctx context.Context, channelID string) ([]ChannelAgentOption, error) {
	agents, err := model.ListActiveByTrigger(ctx, model.TriggerMention)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelAgentOption, 0, len(agents))
	for _, a := range agents {
		ids := parseScope(a).ChannelIDs
		opt := ChannelAgentOption{
			ID:        a.Id.String(),
			Name:      a.Name,
			InChannel: containsChannel(ids, channelID),
			Global:    len(ids) == 0,
		}
		if a.AvatarKey != nil {
			opt.AvatarKey = *a.AvatarKey
		}
		out = append(out, opt)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// ChannelMentionAgent is one agent surfaced in a channel's @mention typeahead,
// shaped like the user-mention entries the composer already renders (so the FE
// can merge it into the same list). Only agents explicitly scoped to the
// channel are returned, so an agent is chip-mentionable exactly where it has
// been added.
type ChannelMentionAgent struct {
	UserUUID   string `json:"user_uuid"`
	Uid        string `json:"uid"`
	UserName   string `json:"user_name"`
	IsBot      bool   `json:"is_bot"`
	ProfileKey string `json:"user_profile_object_key,omitempty"`
}

// ListChannelMentionAgents returns the mention entries for the active agents
// scoped to the given channel (resolved to their bot principals). Powers the
// channel @mention typeahead so agents are discoverable as chips only in the
// channels they have been added to.
func ListChannelMentionAgents(ctx context.Context, channelID string) ([]ChannelMentionAgent, error) {
	agents, err := model.ListActiveByTrigger(ctx, model.TriggerMention)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelMentionAgent, 0)
	for _, a := range agents {
		if !containsChannel(parseScope(a).ChannelIDs, channelID) {
			continue
		}
		bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
		if berr != nil || bot == nil || bot.DgraphUID == "" {
			continue
		}
		out = append(out, ChannelMentionAgent{
			UserUUID:   bot.UUID,
			Uid:        bot.DgraphUID,
			UserName:   a.Name,
			IsBot:      true,
			ProfileKey: bot.ProfileKey,
		})
	}
	return out, nil
}

// it does (or does not) respond to @mentions there. Idempotent; reloads the
// trigger cache so the change takes effect immediately.
func SetAgentChannelMembership(ctx context.Context, agentID uuid.UUID, channelID string, enabled bool) error {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return errNotFound
	}
	a, err := model.GetAgentByID(ctx, agentID)
	if err != nil {
		return err
	}
	if a == nil {
		return errNotFound
	}

	ids := parseScope(a).ChannelIDs
	next, changed := applyChannelMembership(ids, channelID, enabled)
	if !changed {
		return nil // already in the desired state
	}

	scopeJSON, merr := marshalScope(a.Scope, next)
	if merr != nil {
		return merr
	}
	if err := model.SetAgentScope(ctx, agentID, scopeJSON); err != nil {
		return err
	}

	// Resolve the agent's bot principal (provisioning on first add) so we can
	// (a) persist the denormalized link, (b) make it chip-mentionable, and
	// (c) maintain the REAL channel-membership edge (Req 8): on add it becomes a
	// queryable ch_members member that shows in the roster; on remove the edge
	// is dropped. The scope (kept in sync above) drives the mention-response
	// gate; the edge makes "the AI is in this channel" true in the graph.
	bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
	if berr != nil || bot == nil || bot.DgraphUID == "" {
		if berr != nil {
			helpers.LogErrorWithContext(ctx, "business/SetAgentChannelMembership ensure principal failed: %+v", berr)
		}
	} else {
		if a.BotUserId == nil || *a.BotUserId != bot.UserID {
			if serr := model.SetAgentBotUser(ctx, a.Id, bot.UserID); serr != nil {
				helpers.LogErrorWithContext(ctx, "business/SetAgentChannelMembership persist bot_user_id failed: %+v", serr)
			}
		}
		if chUUID, perr := uuid.Parse(channelID); perr == nil {
			if enabled {
				if eerr := channelBusiness.AddChannelBotMemberEdge(ctx, chUUID, bot.DgraphUID); eerr != nil {
					helpers.LogErrorWithContext(ctx, "business/SetAgentChannelMembership add member edge failed: %+v", eerr)
				}
			} else {
				if eerr := channelBusiness.RemoveChannelBotMemberEdge(ctx, chUUID, bot.DgraphUID); eerr != nil {
					helpers.LogErrorWithContext(ctx, "business/SetAgentChannelMembership remove member edge failed: %+v", eerr)
				}
			}
		}
	}

	go ReloadTriggerCache(context.WithoutCancel(ctx))
	return nil
}

// applyChannelMembership returns the channel id list after adding/removing
// channelID, and whether anything changed.
func applyChannelMembership(ids []string, channelID string, enabled bool) ([]string, bool) {
	has := containsChannel(ids, channelID)
	if enabled == has {
		return ids, false
	}
	if enabled {
		return append(cleanChannelIDs(ids), channelID), true
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != channelID {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out, true
}

// marshalScope re-serializes an agent's scope JSON with a replaced channel list,
// preserving any other keys already present (e.g. project_ids).
func marshalScope(existingScopeJSON string, channelIDs []string) (string, error) {
	m := map[string]interface{}{}
	if strings.TrimSpace(existingScopeJSON) != "" {
		if err := json.Unmarshal([]byte(existingScopeJSON), &m); err != nil {
			// Corrupt scope: start fresh rather than fail the toggle.
			m = map[string]interface{}{}
		}
	}
	if len(channelIDs) == 0 {
		delete(m, "channel_ids")
	} else {
		m["channel_ids"] = channelIDs
	}
	b, err := json.Marshal(m)
	if err != nil {
		helpers.LogErrorWithContext(context.Background(), "business/marshalScope failed: %+v", err)
		return "", err
	}
	return string(b), nil
}

func containsChannel(ids []string, channelID string) bool {
	for _, id := range ids {
		if strings.TrimSpace(id) == channelID {
			return true
		}
	}
	return false
}

// cleanChannelIDs trims and drops blanks/dupes, preserving order.
func cleanChannelIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
