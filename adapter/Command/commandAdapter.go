// Package adapter (Command) holds the request/response DTOs for the
// user-facing slash command framework. These shapes are intentionally
// Slack-Block-Kit compatible so existing Slack app handlers can target
// OneCamp with minimal changes, and so the FE can render interactive cards.
package adapter

// ExecuteCommandRequest is the body of POST /command/execute.
type ExecuteCommandRequest struct {
	Command   string  `json:"command"`               // "/giphy" or "giphy"
	Text      string  `json:"text"`                  // raw args after the command
	ChannelID *string `json:"channel_id,omitempty"`  // channel UUID when invoked in a channel
	DmGroupID *string `json:"dm_group_id,omitempty"` // grouping id when invoked in a DM/group
	ThreadTs  *string `json:"thread_ts,omitempty"`   // optional thread anchor
	Timezone  string  `json:"timezone,omitempty"`    // IANA tz, for /remind time parsing
	TriggerID string  `json:"trigger_id,omitempty"`  // client-generated correlation id
}

// InteractRequest is the body of POST /command/interact (a button/select click
// on an interactive card).
type InteractRequest struct {
	TriggerID string            `json:"trigger_id"`        // correlates to the card
	ActionID  string            `json:"action_id"`         // which element was activated
	Value     string            `json:"value,omitempty"`   // element value (e.g. selected gif id)
	Command   string            `json:"command,omitempty"` // originating command
	State     map[string]string `json:"state,omitempty"`   // opaque card state round-tripped
	ChannelID *string           `json:"channel_id,omitempty"`
	DmGroupID *string           `json:"dm_group_id,omitempty"`
}

// CommandResponse is returned by /command/execute and /command/interact, and is
// also the shape pushed over MQTT for async (deferred/external) results.
type CommandResponse struct {
	// ResponseType: "ephemeral" (only invoker sees it) | "in_channel" (posted).
	ResponseType string  `json:"response_type"`
	Text         string  `json:"text,omitempty"`
	Blocks       []Block `json:"blocks,omitempty"`
	// TriggerID echoes the request id so the FE can match an async MQTT push
	// to the card it is currently showing.
	TriggerID string `json:"trigger_id,omitempty"`
	// ReplaceOriginal asks the FE to swap the existing card in place (used by
	// interactive round-trips like Giphy "shuffle").
	ReplaceOriginal bool `json:"replace_original,omitempty"`
	// Ephemeral indicates the card should not be persisted as a message.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// ClientAction is a typed directive the FE performs for commands that are
	// fundamentally client UI actions (open search, toggle media, prefill the
	// composer, open a DM). This mirrors how many Slack slash commands work
	// and keeps navigation/display logic out of the backend.
	ClientAction *ClientAction `json:"client_action,omitempty"`
	// PreloadURLs lists media URLs the FE should warm in the browser cache
	// (e.g. the full Giphy result set), so an interactive shuffle swaps to an
	// already-downloaded image instantly instead of fetching on each click.
	PreloadURLs []string `json:"preload_urls,omitempty"`
}

// ClientAction is a front-end directive carried on a CommandResponse.
type ClientAction struct {
	// Type: "set_composer" | "open_search" | "open_shortcuts" | "toggle_media"
	//       | "open_dm" | "open_channel" | "navigate" | "post_message"
	Type string `json:"type"`
	// Payload carries action-specific fields (text, query, href, target, etc.).
	Payload map[string]string `json:"payload,omitempty"`
}

// CatalogResponse is returned by GET /command/catalog — the scoped list the
// composer typeahead renders.
type CatalogResponse struct {
	Commands []CatalogCommand `json:"commands"`
}

// CatalogCommand is one entry in the typeahead.
type CatalogCommand struct {
	Command     string `json:"command"` // "remind" (no slash)
	Description string `json:"description"`
	UsageHint   string `json:"usage_hint,omitempty"`
	AppSlug     string `json:"app_slug,omitempty"`
	AppName     string `json:"app_name,omitempty"`
	IconURL     string `json:"icon_url,omitempty"`
	IsBuiltin   bool   `json:"is_builtin"`
}

// --- Block Kit (Slack-compatible subset) ---

// Block is a layout block of an interactive/rich response.
type Block struct {
	Type     string         `json:"type"` // section | divider | header | context | image | actions
	Text     *BlockText     `json:"text,omitempty"`
	Fields   []BlockText    `json:"fields,omitempty"`
	ImageURL string         `json:"image_url,omitempty"`
	AltText  string         `json:"alt_text,omitempty"`
	Title    *BlockText     `json:"title,omitempty"`
	Elements []BlockElement `json:"elements,omitempty"` // for type=actions
	BlockID  string         `json:"block_id,omitempty"`
}

// BlockText is a text object (plain_text | mrkdwn).
type BlockText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// BlockElement is an interactive element inside an actions block.
type BlockElement struct {
	Type     string        `json:"type"` // button | select
	Text     *BlockText    `json:"text,omitempty"`
	ActionID string        `json:"action_id"` // identifies the click for /command/interact
	Value    string        `json:"value,omitempty"`
	Style    string        `json:"style,omitempty"`   // default | primary | danger
	URL      string        `json:"url,omitempty"`     // link buttons
	Options  []BlockOption `json:"options,omitempty"` // for select
}

// BlockOption is one option in a select element.
type BlockOption struct {
	Text  string `json:"text"`
	Value string `json:"value"`
}
