package business

// Task properties by name: what a form's question fills (business/Form
// MapsTo) and what a template asks its maker to fill (business/TaskTemplate
// Required) say it in the same words, so a required property means one thing
// wherever a task is made.

import (
	"bytes"
	"html"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// The properties a task to make can be asked for.
const (
	PropDescription = "description"
	PropPriority    = "priority"
	PropDue         = "due"
	PropStart       = "start"
	PropTags        = "tags"
	PropAssignee    = "assignee"
	PropType        = "type"
	// PropField names one of the project's task fields: PropField and its id.
	PropField = "field:"
)

var props = map[string]bool{
	PropDescription: true, PropPriority: true, PropDue: true, PropStart: true,
	PropTags: true, PropAssignee: true, PropType: true,
}

// PropOf is a property's key as it is kept, or "" when it names none: a
// known property in lower case, or a field's prefix and id. Pure.
func PropOf(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if props[k] {
		return k
	}
	if rest, ok := strings.CutPrefix(k, PropField); ok {
		if id, err := uuid.Parse(rest); err == nil {
			return PropField + id.String()
		}
	}
	return ""
}

// Missing is the properties of required a task to make leaves empty, in the
// order asked: a description with no words in it is empty, and so is a
// field whose value is null. A key that names no property is never filled.
// Pure.
func Missing(required []string, in NewTask) []string {
	var out []string
	for _, key := range required {
		if !filled(PropOf(key), in) {
			out = append(out, key)
		}
	}
	return out
}

func filled(prop string, in NewTask) bool {
	switch prop {
	case PropDescription:
		return hasWords(in.Description)
	case PropPriority:
		return strings.TrimSpace(in.Priority) != ""
	case PropDue:
		return in.Due != nil
	case PropStart:
		return in.Start != nil
	case PropTags:
		return strings.Trim(in.Label, " ,") != ""
	case PropAssignee:
		return in.Assignee != nil && in.Assignee.Uuid != ""
	case PropType:
		return in.Type != nil && *in.Type != uuid.Nil
	}
	rest, ok := strings.CutPrefix(prop, PropField)
	if !ok {
		return false
	}
	id, err := uuid.Parse(rest)
	if err != nil {
		return false
	}
	v, ok := in.Fields[id]
	return ok && !IsNull(v)
}

// IsNull says whether a value as a field keeps it is no value. Pure.
func IsNull(v []byte) bool {
	v = bytes.TrimSpace(v)
	return len(v) == 0 || string(v) == "null"
}

var tags = regexp.MustCompile(`<[^>]*>`)

// hasWords says whether the editor's HTML holds anything: words, or a
// picture. Pure.
func hasWords(s string) bool {
	if strings.Contains(s, "<img") {
		return true
	}
	text := html.UnescapeString(tags.ReplaceAllString(s, " "))
	return strings.TrimSpace(strings.ReplaceAll(text, " ", " ")) != ""
}
