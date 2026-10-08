package jira

// Jira's custom fields belong to the whole site, not to a project, and a
// site has many that a project never uses. So a field is made on a
// project the first time one of its issues has a value of it, and
// SourceProject.Fields stays empty. The site's field list (GET
// /rest/api/3/field, read once an import) says what each customfield_NNNNN
// is; the kinds OneCamp has a field for are asked for with each issue:
//
//	select, radio buttons        {"id":"10001","value":"High"}
//	multi-select, checkboxes     [{"id":"10001","value":"Web"}, …]
//	labels                       ["launch", …]
//	number, story points         5
//	text, paragraph              "…", or a document (ADF) for a paragraph
//	URL                          "https://…"
//	date, date and time          "2026-11-02", "2026-11-02T09:00:00.000+0530"
//	user, users                  {"accountId":"…"}, [{"accountId":"…"}, …]
//
// Sprints, epics, ranks and the like are Jira's own workings and aren't
// brought across.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

type jiraFieldMeta struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Custom bool   `json:"custom"`
	Schema *struct {
		Type   string `json:"type"`
		Custom string `json:"custom"`
	} `json:"schema"`
}

const fieldTypes = "com.atlassian.jira.plugin.system.customfieldtypes:"

// kindOf is the OneCamp field type of a Jira custom field, "" when there's
// none.
func kindOf(m jiraFieldMeta) string {
	if !m.Custom || m.Schema == nil || strings.TrimSpace(m.Name) == "" {
		return ""
	}
	switch strings.TrimPrefix(m.Schema.Custom, fieldTypes) {
	case "select", "radiobuttons":
		return importProvider.FieldSelect
	case "multiselect", "multicheckboxes", "labels":
		return importProvider.FieldMultiSelect
	case "float", "com.pyxis.greenhopper.jira:jsw-story-points":
		return importProvider.FieldNumber
	case "textfield", "textarea":
		return importProvider.FieldText
	case "url":
		return importProvider.FieldURL
	case "datepicker", "datetime":
		return importProvider.FieldDate
	case "userpicker", "multiuserpicker":
		return importProvider.FieldPerson
	}
	return ""
}

// siteFields is a site's custom fields that OneCamp has a kind for, by id.
type siteFields map[string]jiraFieldMeta

// customFields is the site's custom fields OneCamp can bring across, read
// once for each import. A token that may not read the list (401, 403, 404)
// is said once, and the import goes on without custom fields; anything else
// (Jira asking to slow down, a server error, a timeout) is the chunk's to
// try again.
func (p *Provider) customFields(ctx context.Context, j *importModels.Job, tok, site string) (siteFields, error) {
	p.mu.Lock()
	cached, ok := p.fieldsCache[j.Id]
	p.mu.Unlock()
	if ok {
		return cached, nil
	}
	var metas []jiraFieldMeta
	fields := siteFields{}
	if err := p.getJSON(ctx, tok, site+"/rest/api/3/field", &metas); err != nil {
		var status *statusError
		if !errors.As(err, &status) || (status.Code != 401 && status.Code != 403 && status.Code != 404) {
			return nil, err // slow down, a server error or a timeout: the chunk tries again
		}
		importModels.LogImportError(ctx, j.Id, nil, importModels.EntityField, "jira-fields", importModels.SeverityWarning, "FIELDS_UNREADABLE",
			"Jira's list of custom fields couldn't be read with this token, so custom fields weren't brought across: "+err.Error(), nil)
	}
	for _, m := range metas {
		if kindOf(m) != "" {
			fields[m.ID] = m
		}
	}
	p.mu.Lock()
	p.fieldsCache[j.Id] = fields
	p.mu.Unlock()
	return fields, nil
}

// issueFieldValues is an issue's values of the site's custom fields.
func issueFieldValues(custom map[string]json.RawMessage, fields siteFields) []importProvider.SourceFieldValue {
	ids := make([]string, 0, len(custom))
	for id := range custom {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []importProvider.SourceFieldValue
	for _, id := range ids {
		m, ok := fields[id]
		if !ok {
			continue
		}
		f := importProvider.SourceField{SourceID: m.ID, Name: strings.TrimSpace(m.Name), Type: kindOf(m)}
		if v := fieldValue(custom[id], &f); v != nil {
			out = append(out, importProvider.SourceFieldValue{Field: f, Value: v, Text: peopleNames(custom[id], f.Type)})
		}
	}
	return out
}

// peopleNames is the names a user or users field shows, for when the people
// themselves can't come across.
func peopleNames(raw json.RawMessage, kind string) string {
	if kind != importProvider.FieldPerson {
		return ""
	}
	type person struct {
		DisplayName string `json:"displayName"`
	}
	var one person
	var many []person
	if json.Unmarshal(raw, &one) == nil && one.DisplayName != "" {
		return one.DisplayName
	}
	var names []string
	if json.Unmarshal(raw, &many) == nil {
		for _, p := range many {
			if p.DisplayName != "" {
				names = append(names, p.DisplayName)
			}
		}
	}
	return strings.Join(names, ", ")
}

type jiraOption struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// fieldValue is a raw value as its field holds it, nil when there's none;
// a choice's options are added to f as they're met.
func fieldValue(raw json.RawMessage, f *importProvider.SourceField) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	option := func(id, label string) string {
		f.Options = append(f.Options, importProvider.SourceOption{SourceID: id, Label: label})
		return id
	}
	switch f.Type {
	case importProvider.FieldSelect:
		var o jiraOption
		if json.Unmarshal(raw, &o) == nil && o.ID != "" {
			return option(o.ID, o.Value)
		}
	case importProvider.FieldMultiSelect:
		var ids []string
		var opts []jiraOption
		var labels []string
		if json.Unmarshal(raw, &opts) == nil {
			for _, o := range opts {
				if o.ID != "" {
					ids = append(ids, option(o.ID, o.Value))
				}
			}
		} else if json.Unmarshal(raw, &labels) == nil {
			for _, l := range labels {
				if l = strings.TrimSpace(l); l != "" {
					ids = append(ids, option(l, l))
				}
			}
		}
		if len(ids) > 0 {
			return ids
		}
	case importProvider.FieldNumber:
		var n float64
		if json.Unmarshal(raw, &n) == nil {
			return n
		}
	case importProvider.FieldText:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			var doc any
			if json.Unmarshal(raw, &doc) == nil {
				s = extractAtlassianText(doc)
			}
		}
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	case importProvider.FieldURL:
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	case importProvider.FieldDate:
		var s string
		if json.Unmarshal(raw, &s) == nil && len(s) >= 10 {
			return s[:10]
		}
	case importProvider.FieldPerson:
		type person struct {
			AccountID string `json:"accountId"`
		}
		var one person
		var many []person
		if json.Unmarshal(raw, &one) == nil && one.AccountID != "" {
			return one.AccountID
		}
		if json.Unmarshal(raw, &many) == nil && len(many) > 0 && many[0].AccountID != "" {
			return many[0].AccountID
		}
	}
	return nil
}
