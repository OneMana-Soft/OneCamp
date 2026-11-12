package contentcodec

import (
	"regexp"
	"strings"
)

// Mentions written as text (the public API's way to mention someone, as
// Airtable's API writes @[usrID]): a person is @[<their user_uuid>].
// WriteMentions makes each such token the person's mention, the chip the
// app's composer writes (data-id "<uuid>@<graph uid>", data-label their name,
// which helpers.GetMentions reads), so they are told as anyone mentioned in
// the app is; MentionsAsTokens writes each mention back as its token, so what
// an API caller wrote is what it reads, and no graph uid leaves the server.

// Mention is a person a body mentions: their uuid, their own graph node and
// the name their chip shows.
type Mention struct {
	UUID  string
	UID   string
	Label string
}

// mentionToken is @[<uuid>], any case.
var mentionToken = regexp.MustCompile(`@\[([0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})\]`)

// MentionToken is how text names a person: @[uuid].
func MentionToken(uuid string) string { return "@[" + strings.ToLower(uuid) + "]" }

// MentionTokens is the people text names as @[uuid], each once, lower case,
// in the order first written.
func MentionTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionToken.FindAllStringSubmatch(text, -1) {
		if id := strings.ToLower(m[1]); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// WriteMentions makes each @[uuid] in doc's text whose person people holds
// (by lower-case uuid) that person's mention. Any other @[…] stays text, as
// does one in code or in a link's text. It changes doc in place and returns it.
func WriteMentions(doc *Node, people map[string]Mention) *Node {
	if doc == nil || len(people) == 0 {
		return doc
	}
	writeMentions(doc, people)
	return doc
}

func writeMentions(n *Node, people map[string]Mention) {
	if n.Type == "codeBlock" || len(n.Content) == 0 {
		return
	}
	var out []*Node
	changed := false
	for _, c := range n.Content {
		if c.Type != "text" || hasMark(c.Marks, "code") || hasMark(c.Marks, "link") {
			writeMentions(c, people)
			out = append(out, c)
			continue
		}
		last, split := 0, false
		for _, at := range mentionToken.FindAllStringSubmatchIndex(c.Text, -1) {
			p, ok := people[strings.ToLower(c.Text[at[2]:at[3]])]
			if !ok || p.UID == "" {
				continue
			}
			if at[0] > last {
				out = append(out, &Node{Type: "text", Text: c.Text[last:at[0]], Marks: c.Marks})
			}
			m := newNode("mention")
			m.setAttr("id", strings.ToLower(p.UUID)+"@"+p.UID)
			if p.Label != "" {
				m.setAttr("label", p.Label)
			}
			m.setAttr("mentionSuggestionChar", "@")
			out = append(out, m)
			last, split = at[1], true
		}
		if !split {
			out = append(out, c)
			continue
		}
		changed = true
		if last < len(c.Text) {
			out = append(out, &Node{Type: "text", Text: c.Text[last:], Marks: c.Marks})
		}
	}
	if changed {
		n.Content = out
	}
}

// MentionsAsTokens writes each person's mention in doc back as the text
// @[uuid], and is the people it mentioned, each once, in order, with the name
// their chip kept. A mention whose id isn't a person's ("<uuid>@<graph uid>")
// stays as it is. It changes doc in place.
func MentionsAsTokens(doc *Node) []Mention {
	var found []Mention
	seen := map[string]bool{}
	mentionsAsTokens(doc, &found, seen)
	return found
}

func mentionsAsTokens(n *Node, found *[]Mention, seen map[string]bool) {
	if n == nil {
		return
	}
	for i, c := range n.Content {
		if c.Type != "mention" {
			mentionsAsTokens(c, found, seen)
			continue
		}
		id, uid, ok := strings.Cut(c.attrString("id"), "@")
		if !ok || uid == "" || !mentionToken.MatchString("@["+id+"]") {
			continue
		}
		id = strings.ToLower(id)
		if !seen[id] {
			seen[id] = true
			*found = append(*found, Mention{UUID: id, UID: uid, Label: c.attrString("label")})
		}
		n.Content[i] = &Node{Type: "text", Text: MentionToken(id), Marks: c.Marks}
	}
}
