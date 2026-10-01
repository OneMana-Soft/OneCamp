package business

import (
	"regexp"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// OneCamp links in a task, turned into what the agent's tools accept.
//
// A task made from a message says "From Sam Rivera's message" and links to it.
// The task's description reached the agent as plain text, which dropped every
// link target, so the agent could not know which message was meant and asked
// "Which channel contains Sam Rivera's message?". The person had already told
// it. Each in-app link now arrives as the ids the matching tool takes, so the
// agent sees what the person sees and looks it up instead of asking.

var appLinkPattern = regexp.MustCompile(`href="[^"]*?(/app/(?:channel|chat|doc|task)/[A-Za-z0-9/_-]+)"`)

const maxAppLinkRefs = 8

// appLinkRefs describes each distinct in-app link in html as the ids its tool
// takes, in the order they appear. Pure.
func appLinkRefs(html string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range appLinkPattern.FindAllStringSubmatch(html, -1) {
		path := strings.TrimRight(m[1], "/")
		if seen[path] {
			continue
		}
		seen[path] = true
		if ref := describeAppLink(path); ref != "" {
			out = append(out, ref)
		}
		if len(out) == maxAppLinkRefs {
			break
		}
	}
	return out
}

func describeAppLink(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/app/"), "/")
	switch parts[0] {
	case "channel":
		if len(parts) >= 3 {
			return "a message in a channel: channel_uuid=" + parts[1] + " post_uuid=" + parts[2] + " (summarize_channel reads that channel)"
		}
		if len(parts) == 2 {
			return "a channel: channel_uuid=" + parts[1] + " (summarize_channel reads it)"
		}
	case "chat":
		if len(parts) >= 4 && parts[1] == "group" {
			return "a message in a group chat: group chat id=" + parts[2] + " message=" + parts[3] + " (summarize_group_chat reads it)"
		}
		if len(parts) >= 3 {
			return "a message in a direct message with user " + parts[1] + ": message=" + parts[2] + " (summarize_dm reads it)"
		}
	case "doc":
		if len(parts) >= 2 {
			return "a document: doc_uuid=" + parts[1] + " (read_doc reads it)"
		}
	case "task":
		if len(parts) >= 2 {
			return "a task: task_uuid=" + parts[1] + " (list_project_tasks and the task tools act on it)"
		}
	}
	return ""
}

// promptText is html as the agent reads it: plain text, followed by the in-app
// links it contains as tool ids. Flattening alone keeps the words and loses
// where they point.
func promptText(html string) string {
	text := strings.TrimSpace(helpers.HTMLToPlainText(html))
	refs := appLinkRefs(html)
	if len(refs) == 0 {
		return text
	}
	return text + "\n(Links in this: " + strings.Join(refs, "; ") + ")"
}
