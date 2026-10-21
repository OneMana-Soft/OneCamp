// Package business (Cards) is Interactive Cards (#34; platform P-API,
// design/platform/API_CONTRACT.md §3.5): the one card model every surface
// draws (a command's answer, an app's message, an incoming webhook's blocks),
// the subset an app may send, and the actions on a card, checked on the
// server and routed to whoever owns them.
//
// A card is a short list of blocks in the shape the web's card view draws
// (Slack's Block Kit, narrowed): section, header, context, image, divider and
// actions, whose elements are buttons, selects and overflow menus. Whatever an
// app sends goes through Reduce, which keeps the subset, within its limits,
// and says what it dropped.
package business

// The limits of a card (§3.5): what Reduce keeps, at most.
const (
	MaxBlocks   = 50
	MaxActions  = 25
	MaxText     = 3000
	MaxHeader   = 150
	MaxFields   = 10
	MaxOptions  = 100
	MaxOverflow = 5
	MaxActionID = 255
	MaxValue    = 2000
	MaxAltText  = 2000
)

// Block kinds.
const (
	BlockSection = "section"
	BlockHeader  = "header"
	BlockContext = "context"
	BlockImage   = "image"
	BlockDivider = "divider"
	BlockActions = "actions"
)

// Element kinds, inside an actions block.
const (
	ElementButton   = "button"
	ElementSelect   = "select"
	ElementOverflow = "overflow"
)

// Text kinds.
const (
	TextPlain    = "plain_text"
	TextMarkdown = "mrkdwn"
)

// Card is what a surface draws.
type Card struct {
	Version int     `json:"version"`
	Blocks  []Block `json:"blocks"`
}

// Block is one of a card's blocks. Its JSON is the shape the web app's card
// view (and adapter/Command's Block) has always drawn.
type Block struct {
	Type     string    `json:"type"`
	Text     *Text     `json:"text,omitempty"`
	Fields   []Text    `json:"fields,omitempty"`
	ImageURL string    `json:"image_url,omitempty"`
	AltText  string    `json:"alt_text,omitempty"`
	Title    *Text     `json:"title,omitempty"`
	Elements []Element `json:"elements,omitempty"`
	BlockID  string    `json:"block_id,omitempty"`
}

// Text is a text object.
type Text struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Element is an interactive element of an actions block, or a context
// block's text.
type Element struct {
	Type     string   `json:"type"`
	Text     *Text    `json:"text,omitempty"`
	ActionID string   `json:"action_id,omitempty"`
	Value    string   `json:"value,omitempty"`
	Style    string   `json:"style,omitempty"`
	URL      string   `json:"url,omitempty"`
	Options  []Option `json:"options,omitempty"`
}

// Option is one choice of a select or an overflow menu.
type Option struct {
	Text  string `json:"text"`
	Value string `json:"value"`
}

// CardVersion is the card model's version: a card that says a later one is
// drawn as far as this one goes.
const CardVersion = 1

// Actions are the card's interactive elements, in order.
func (c Card) Actions() []Element {
	var out []Element
	for _, b := range c.Blocks {
		if b.Type == BlockActions {
			out = append(out, b.Elements...)
		}
	}
	return out
}
