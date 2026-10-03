package business

// Polls: a question in a channel that everyone can answer in one click, with
// the results live for everyone. Made by a person (/poll) or by an agent
// (create_poll), under the same rules as posting in that channel.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Limits a poll is held to. Small on purpose: a poll is a quick question, and
// these are also what keeps an agent from posting a wall of options.
const (
	MaxOptions     = 10
	MaxOptionRunes = 100
	MaxQuestion    = 300
	MaxOpenHours   = 24 * 30
)

// InputError is a request the person can fix by changing what they sent.
type InputError string

func (e InputError) Error() string { return string(e) }

// ErrNoAccess hides whether a poll exists from someone outside its channel.
var ErrNoAccess = errors.New("poll not found or you don't have access")

// NewPoll is what a person or an agent asks for.
type NewPoll struct {
	ChannelUUID string
	Question    string
	Options     []string
	Multiple    bool
	// OpenHours closes voting after this many hours; 0 leaves it open.
	OpenHours int
	// AIGenerated marks the message as written by an agent (AI Act provenance).
	AIGenerated bool
}

// Normalize trims and validates a request. Pure, for its test.
func (n NewPoll) Normalize() (NewPoll, error) {
	n.Question = strings.TrimSpace(n.Question)
	if n.Question == "" {
		return n, InputError("a poll needs a question")
	}
	if len([]rune(n.Question)) > MaxQuestion {
		return n, InputError(fmt.Sprintf("the question is longer than %d characters", MaxQuestion))
	}
	seen := map[string]bool{}
	var opts []string
	for _, o := range n.Options {
		o = strings.TrimSpace(o)
		if o == "" || seen[strings.ToLower(o)] {
			continue
		}
		if len([]rune(o)) > MaxOptionRunes {
			return n, InputError(fmt.Sprintf("an option is longer than %d characters", MaxOptionRunes))
		}
		seen[strings.ToLower(o)] = true
		opts = append(opts, o)
	}
	if len(opts) < 2 {
		return n, InputError("a poll needs at least two different options")
	}
	if len(opts) > MaxOptions {
		return n, InputError(fmt.Sprintf("a poll can have at most %d options", MaxOptions))
	}
	n.Options = opts
	if n.OpenHours < 0 || n.OpenHours > MaxOpenHours {
		return n, InputError("a poll can stay open for at most 30 days")
	}
	return n, nil
}

// SplitOptions reads options an agent wrote as one string: one per line, or
// separated by "|" or ";". Commas are left alone, since options contain them.
func SplitOptions(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '|' || r == ';' })
}

// channelFor checks that the user may read (and, to create, post in) a channel.
func channelFor(ctx context.Context, channelUUID string, user *userModels.UserInfo, toPost bool) (*dgraphStruct.DgraphChannel, error) {
	ch, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, user.UserDgraphInfo.Uid)
	if err != nil || ch == nil || ch.Uuid == "" || (ch.DeletedAt != nil && !ch.DeletedAt.IsZero()) || ch.IsMember == 0 {
		return nil, ErrNoAccess
	}
	if toPost && ch.PostPolicy == "admins_only" && ch.IsAdmin == 0 {
		return nil, InputError("only channel admins can post in this announcement channel")
	}
	return ch, nil
}

// PollHTML is the message body: the question as text for notifications,
// search and the Slack bridge, then the block the web app renders as buttons.
func PollHTML(pollID uuid.UUID, question string) string {
	return "<p><strong>Poll:</strong> " + html.EscapeString(question) + "</p>" +
		`<div data-type="poll" data-id="` + pollID.String() + `"></div>`
}

// Create stores a poll and posts it in its channel as the user.
func Create(ctx context.Context, user *userModels.UserInfo, in NewPoll) (*View, error) {
	in, err := in.Normalize()
	if err != nil {
		return nil, err
	}
	channelID, err := uuid.Parse(in.ChannelUUID)
	if err != nil {
		return nil, InputError("not a channel id")
	}
	ch, err := channelFor(ctx, in.ChannelUUID, user, true)
	if err != nil {
		return nil, err
	}
	p := &pollModel.Poll{
		ID: uuid.New(), ChannelID: channelID, Question: in.Question, Multiple: in.Multiple,
		CreatedBy: user.UserPostgresInfo.Id,
	}
	for i, o := range in.Options {
		p.Options = append(p.Options, pollModel.Option{ID: strconv.Itoa(i + 1), Text: o})
	}
	if in.OpenHours > 0 {
		t := time.Now().Add(time.Duration(in.OpenHours) * time.Hour)
		p.ClosesAt = &t
	}
	if err := pollModel.Create(ctx, p); err != nil {
		return nil, fmt.Errorf("store poll: %w", err)
	}
	body := PollHTML(p.ID, p.Question)
	if in.AIGenerated {
		body = `<div data-ai-generated="true">` + body + `</div>`
	}
	post, err := postBusiness.CreatePost(ctx, &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText: body, ChannelUuid: in.ChannelUUID, ChannelUUID: channelID,
	}, user, nil, ch)
	if err != nil {
		_ = pollModel.Delete(ctx, p.ID)
		return nil, fmt.Errorf("post poll: %w", err)
	}
	if postID, perr := uuid.Parse(post.Uuid); perr == nil {
		_ = pollModel.SetPost(ctx, p.ID, postID)
		p.PostID = &postID
	}
	return view(p, &pollModel.Tally{Counts: map[string]int{}}, user.UserPostgresInfo.Id, true), nil
}

// View is a poll as one reader sees it.
type View struct {
	ID        string       `json:"id"`
	Channel   string       `json:"channel_uuid"`
	PostID    string       `json:"post_uuid,omitempty"`
	Question  string       `json:"question"`
	Multiple  bool         `json:"multiple"`
	Options   []OptionView `json:"options"`
	Voters    int          `json:"voters"`
	Mine      []string     `json:"mine"`
	ClosesAt  *time.Time   `json:"closes_at,omitempty"`
	Closed    bool         `json:"closed"`
	CanClose  bool         `json:"can_close"`
	CreatedBy string       `json:"created_by"`
}

// OptionView is one option with its count.
type OptionView struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Votes int    `json:"votes"`
}

func isClosed(p *pollModel.Poll, now time.Time) bool {
	return p.ClosedAt != nil || (p.ClosesAt != nil && !now.Before(*p.ClosesAt))
}

func view(p *pollModel.Poll, t *pollModel.Tally, reader uuid.UUID, canClose bool) *View {
	v := &View{
		ID: p.ID.String(), Channel: p.ChannelID.String(), Question: p.Question, Multiple: p.Multiple,
		Voters: t.Voters, Mine: t.Mine, ClosesAt: p.ClosesAt, Closed: isClosed(p, time.Now()),
		CanClose: canClose, CreatedBy: p.CreatedBy.String(),
	}
	if v.Mine == nil {
		v.Mine = []string{}
	}
	if p.PostID != nil {
		v.PostID = p.PostID.String()
	}
	for _, o := range p.Options {
		v.Options = append(v.Options, OptionView{ID: o.ID, Text: o.Text, Votes: t.Counts[o.ID]})
	}
	return v
}

// load fetches a poll the user may read, with the channel it lives in.
func load(ctx context.Context, user *userModels.UserInfo, pollID string) (*pollModel.Poll, *dgraphStruct.DgraphChannel, error) {
	id, err := uuid.Parse(pollID)
	if err != nil {
		return nil, nil, ErrNoAccess
	}
	p, err := pollModel.Get(ctx, id)
	if errors.Is(err, pollModel.ErrNotFound) {
		return nil, nil, ErrNoAccess
	}
	if err != nil {
		return nil, nil, err
	}
	ch, err := channelFor(ctx, p.ChannelID.String(), user, false)
	if err != nil {
		return nil, nil, err
	}
	return p, ch, nil
}

func canClose(p *pollModel.Poll, ch *dgraphStruct.DgraphChannel, user *userModels.UserInfo) bool {
	return p.CreatedBy == user.UserPostgresInfo.Id || ch.IsAdmin > 0
}

// Get returns a poll as this user sees it.
func Get(ctx context.Context, user *userModels.UserInfo, pollID string) (*View, error) {
	p, ch, err := load(ctx, user, pollID)
	if err != nil {
		return nil, err
	}
	t, err := pollModel.Count(ctx, p.ID, user.UserPostgresInfo.Id)
	if err != nil {
		return nil, err
	}
	return view(p, t, user.UserPostgresInfo.Id, canClose(p, ch, user)), nil
}

// ValidateChoice checks a vote against a poll. Pure, for its test.
func ValidateChoice(p *pollModel.Poll, optionIDs []string, now time.Time) ([]string, error) {
	if isClosed(p, now) {
		return nil, InputError("this poll is closed")
	}
	valid := map[string]bool{}
	for _, o := range p.Options {
		valid[o.ID] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range optionIDs {
		if !valid[id] {
			return nil, InputError("that option is not in this poll")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if !p.Multiple && len(out) > 1 {
		return nil, InputError("this poll takes one choice")
	}
	return out, nil
}

// Vote sets the user's choices (none retracts their vote) and tells the channel.
func Vote(ctx context.Context, user *userModels.UserInfo, pollID string, optionIDs []string) (*View, error) {
	p, ch, err := load(ctx, user, pollID)
	if err != nil {
		return nil, err
	}
	choice, err := ValidateChoice(p, optionIDs, time.Now())
	if err != nil {
		return nil, err
	}
	if err := pollModel.ReplaceVotes(ctx, p.ID, user.UserPostgresInfo.Id, choice); err != nil {
		return nil, err
	}
	go mqttBusiness.PublishPollUpdate(p.ChannelID.String(), p.ID.String())
	t, err := pollModel.Count(ctx, p.ID, user.UserPostgresInfo.Id)
	if err != nil {
		return nil, err
	}
	return view(p, t, user.UserPostgresInfo.Id, canClose(p, ch, user)), nil
}

// Close ends voting. Only the poll's author or a channel admin may.
func Close(ctx context.Context, user *userModels.UserInfo, pollID string) (*View, error) {
	p, ch, err := load(ctx, user, pollID)
	if err != nil {
		return nil, err
	}
	if !canClose(p, ch, user) {
		return nil, InputError("only the person who made this poll, or a channel admin, can close it")
	}
	if err := pollModel.Close(ctx, p.ID); err != nil {
		return nil, err
	}
	go mqttBusiness.PublishPollUpdate(p.ChannelID.String(), p.ID.String())
	return Get(ctx, user, pollID)
}

// Summary is a poll's results as text, for the model and for notifications.
func Summary(v *View) string {
	var b strings.Builder
	state := "open"
	if v.Closed {
		state = "closed"
	}
	fmt.Fprintf(&b, "Poll (%s, %d voted): %s\n", state, v.Voters, v.Question)
	for _, o := range v.Options {
		fmt.Fprintf(&b, "- %s: %d\n", o.Text, o.Votes)
	}
	return strings.TrimSpace(b.String())
}
