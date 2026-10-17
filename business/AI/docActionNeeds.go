package business

// What each Doc AI action needs on the doc it is asked in (docs incident,
// 14 Oct 2026, coordinator decision D1): the one place that says it. The
// server refuses by it (controllers/AI DocAIComplete and its stream), and the
// web offers by it (GET /ai/status, doc_action_needs), so the two can never
// disagree.
//
// Reading an answer in the panel needs only view: a summary, an explanation,
// an answer to a question about the doc, a translation. Changing the doc
// needs edit: a rewrite, anything written in, carrying on writing, fixes. A
// reading action's answer put into the doc is an edit too, and the editor (read
// only below edit) and the live socket refuse it; this says only who may ask.
//
// Before this, Ask AI was offered only to people who could edit, and the
// server asked nothing of the doc: a member who could read a workspace doc
// found no Ask AI on it and no word of why.

import (
	"strings"

	"github.com/akashc777/OneCamp/business/Access/level"
)

var docActionNeeds = map[string]level.Level{
	// Reading: the answer is read in the panel.
	"summarize": level.View,
	"explain":   level.View,
	"ask":       level.View,
	"translate": level.View,
	// Changing: the panel's own actions.
	"write":       level.Edit,
	"expand":      level.Edit,
	"fix_grammar": level.Edit,
	"shorten":     level.Edit,
	"rewrite":     level.Edit,
	// Changing: the names the docs programme's AI bar (B9) gives them and its
	// other changes, so the list already holds them when it arrives.
	"edit":             level.Edit,
	"improve":          level.Edit,
	"fix":              level.Edit,
	"shorter":          level.Edit,
	"longer":           level.Edit,
	"tone":             level.Edit,
	"simplify":         level.Edit,
	"continue":         level.Edit,
	"draft":            level.Edit,
	"draft_from_tasks": level.Edit,
}

// DocActionNeeds is the least level on a doc that action needs. An action not
// named here needs edit, so a new one reads nothing new until it is placed.
func DocActionNeeds(action string) level.Level {
	if l, ok := docActionNeeds[strings.ToLower(strings.TrimSpace(action))]; ok {
		return l
	}
	return level.Edit
}

// DocActionNeedsByName is every action named here with the level it needs, as
// the web reads it ("summarize": "view"). A fresh map each time.
func DocActionNeedsByName() map[string]string {
	out := make(map[string]string, len(docActionNeeds))
	for name, l := range docActionNeeds {
		out[name] = l.String()
	}
	return out
}
