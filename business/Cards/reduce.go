package business

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Reduce turns whatever an app or a webhook sends as a card (a list of blocks,
// or {"blocks": [...]}, in Block Kit's shape) into the subset a card draws,
// within its limits, and says in words what it left out. It never fails: a
// payload it can't read is an empty card and one reason.
func Reduce(raw json.RawMessage) (Card, []string) {
	card := Card{Version: CardVersion, Blocks: []Block{}}
	var dropped []string
	drop := func(format string, a ...any) { dropped = append(dropped, fmt.Sprintf(format, a...)) }

	var blocks []map[string]json.RawMessage
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return card, nil
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return card, []string{"the blocks aren't a list of objects"}
		}
	default:
		var wrapped struct {
			Blocks []map[string]json.RawMessage `json:"blocks"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return card, []string{"the card isn't JSON with a list of blocks"}
		}
		blocks = wrapped.Blocks
	}

	actions := 0
	for i, rb := range blocks {
		if len(card.Blocks) == MaxBlocks {
			drop("%d blocks over the %d a card holds", len(blocks)-i, MaxBlocks)
			break
		}
		kind := str(rb["type"])
		b := Block{Type: kind, BlockID: clip(str(rb["block_id"]), MaxActionID)}
		switch kind {
		case BlockSection:
			b.Text = textOf(rb["text"], MaxText, drop)
			for _, f := range list(rb["fields"]) {
				if len(b.Fields) == MaxFields {
					drop("a section's fields over %d", MaxFields)
					break
				}
				if t := textOf(f, MaxText, drop); t != nil {
					b.Fields = append(b.Fields, *t)
				}
			}
			if acc := rb["accessory"]; len(acc) > 0 {
				drop("a section's accessory (put its button in an actions block)")
			}
			if b.Text == nil && len(b.Fields) == 0 {
				drop("an empty section")
				continue
			}
		case BlockHeader:
			t := textOf(rb["text"], MaxHeader, drop)
			if t == nil {
				drop("a header with no text")
				continue
			}
			t.Type = TextPlain
			b.Text = t
		case BlockContext:
			for _, e := range list(rb["elements"]) {
				var probe map[string]json.RawMessage
				if json.Unmarshal(e, &probe) != nil {
					continue
				}
				if t := str(probe["type"]); t == TextPlain || t == TextMarkdown {
					if tx := textOf(e, MaxText, drop); tx != nil {
						b.Elements = append(b.Elements, Element{Type: tx.Type, Text: tx})
					}
				} else {
					drop("a context block's %s", orWord(t, "element"))
				}
			}
			// A context block written with one "text" (as OneCamp's incoming
			// webhooks have long taken it) is that text as its one element.
			if len(b.Elements) == 0 {
				if tx := textOf(rb["text"], MaxText, drop); tx != nil {
					b.Elements = append(b.Elements, Element{Type: tx.Type, Text: tx})
				}
			}
			if len(b.Elements) == 0 {
				drop("an empty context block")
				continue
			}
		case BlockImage:
			src := str(rb["image_url"])
			if src == "" {
				src = str(rb["url"])
			}
			if why := imageProblem(src); why != "" {
				drop("an image: %s", why)
				continue
			}
			b.ImageURL = src
			b.AltText = clip(str(rb["alt_text"]), MaxAltText)
			b.Title = textOf(rb["title"], MaxHeader, drop)
		case BlockDivider:
		case BlockActions:
			for _, e := range list(rb["elements"]) {
				if actions == MaxActions {
					drop("actions over the %d a card holds", MaxActions)
					break
				}
				el, why := elementOf(e, drop)
				if why != "" {
					drop("%s", why)
					continue
				}
				b.Elements = append(b.Elements, el)
				actions++
			}
			if len(b.Elements) == 0 {
				drop("an actions block with nothing to press")
				continue
			}
		default:
			drop("a %s block", orWord(kind, "nameless"))
			continue
		}
		card.Blocks = append(card.Blocks, b)
	}
	return card, dropped
}

// elementOf reads one interactive element, or says why it can't be kept.
func elementOf(raw json.RawMessage, drop func(string, ...any)) (Element, string) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Element{}, "an element that isn't an object"
	}
	kind := str(m["type"])
	el := Element{ActionID: clip(str(m["action_id"]), MaxActionID), Value: clip(str(m["value"]), MaxValue)}
	switch kind {
	case ElementButton:
		el.Type = ElementButton
		el.Text = textOf(m["text"], MaxHeader, drop)
		if el.Text == nil {
			return Element{}, "a button with no words"
		}
		el.Text.Type = TextPlain
		if s := str(m["style"]); s == "primary" || s == "danger" {
			el.Style = s
		}
		if u := str(m["url"]); u != "" {
			if why := linkProblem(u); why != "" {
				return Element{}, "a link button: " + why
			}
			el.URL = u
		}
	case ElementSelect, "static_select":
		el.Type = ElementSelect
		el.Options = optionsOf(m["options"], MaxOptions, drop)
		if len(el.Options) == 0 {
			return Element{}, "a select with no options"
		}
		if ph := textOf(m["placeholder"], MaxHeader, drop); ph != nil {
			el.Text = &Text{Type: TextPlain, Text: ph.Text}
		}
	case ElementOverflow:
		el.Type = ElementOverflow
		el.Options = optionsOf(m["options"], MaxOverflow, drop)
		if len(el.Options) == 0 {
			return Element{}, "an overflow menu with no options"
		}
	default:
		return Element{}, fmt.Sprintf("a %s element", orWord(kind, "nameless"))
	}
	if el.ActionID == "" && el.URL == "" {
		return Element{}, fmt.Sprintf("a %s with no action_id", el.Type)
	}
	return el, ""
}

// optionsOf reads a select's options: Block Kit's {text: {text}, value} or a
// plain {text, value}.
func optionsOf(raw json.RawMessage, max int, drop func(string, ...any)) []Option {
	var out []Option
	for _, o := range list(raw) {
		var m map[string]json.RawMessage
		if json.Unmarshal(o, &m) != nil {
			continue
		}
		label := str(m["text"])
		if label == "" {
			if t := textOf(m["text"], MaxHeader, drop); t != nil {
				label = t.Text
			}
		}
		value := clip(str(m["value"]), MaxValue)
		if label == "" || value == "" {
			continue
		}
		if len(out) == max {
			drop("options over the %d a menu holds", max)
			break
		}
		out = append(out, Option{Text: clip(label, MaxHeader), Value: value})
	}
	return out
}

// textOf reads a text object ({type, text}, or a bare string), clipped to max.
func textOf(raw json.RawMessage, max int, drop func(string, ...any)) *Text {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var t Text
	if err := json.Unmarshal(raw, &t); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		t = Text{Type: TextPlain, Text: s}
	}
	if t.Type != TextMarkdown {
		t.Type = TextPlain
	}
	if strings.TrimSpace(t.Text) == "" {
		return nil
	}
	if utf8.RuneCountInString(t.Text) > max {
		drop("text past %d characters, cut", max)
		t.Text = clip(t.Text, max)
	}
	return &t
}

// imageProblem says why an image can't be drawn, or "": the browser fetches
// it, so it must be https, named by a host, and not on a private network.
func imageProblem(raw string) string {
	if raw == "" {
		return "no address"
	}
	return linkProblem(raw)
}

// linkProblem says why an address can't be on a card, or "".
func linkProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "not an address"
	}
	if u.Scheme != "https" {
		return "not https"
	}
	if u.User != nil {
		return "it carries a password"
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return "a private host"
	}
	if a, err := netip.ParseAddr(host); err == nil && (a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified()) {
		return "a private address"
	}
	return ""
}

func str(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func list(raw json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// clip cuts s to max characters, ending in an ellipsis when it was longer.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func orWord(s, word string) string {
	if s == "" {
		return word
	}
	return s
}
