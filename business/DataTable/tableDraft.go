package business

// Natural-language table authoring: turn a plain-English description into a
// real, ready-to-use table (header + typed columns + a few seed rows). Unlike
// the workflow draft (which only pre-fills a form), generating a table is
// cheap, non-destructive, and immediately useful, so we create it directly AS
// the requesting user and return it for them to refine. The model never sees
// or invents ids; row values are keyed by column name and mapped to field ids
// server-side after the columns exist.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// generatedTable is the model's structured output.
type generatedTable struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Fields      []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Options []string `json:"options"`
	} `json:"fields"`
	Rows []map[string]interface{} `json:"rows"`
}

const maxGeneratedRows = 25

// GenerateTable creates a table from a natural-language prompt and returns it.
// workspaceContext, when non-empty, is the same RAG context the AI assistant
// uses (content the caller can access); it lets the model ground columns and
// seed rows in real workspace data. Runs as the actor (rate/breaker bound);
// friendly errors when AI is off or the output cannot be understood.
// Best-effort on seed rows: a bad row is skipped, never failing the whole
// generation.
func GenerateTable(ctx context.Context, actor Actor, prompt string, workspaceContext string) (*model.DataTable, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("describe the table you want")
	}
	if len(prompt) > 2000 {
		prompt = prompt[:2000]
	}

	userUUID := actor.UserID.String()
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}

	userMsg := "Describe the table:\n" + prompt
	if c := strings.TrimSpace(workspaceContext); c != "" {
		// Bound the injected context so the prompt stays within the model window.
		if len(c) > 6000 {
			c = c[:6000]
		}
		userMsg = "Relevant context from the user's workspace (their chats, channels, docs, tasks). Use it to pick the right columns and to seed real example rows when the description refers to existing work; ignore it if it isn't relevant:\n" +
			c + "\n\n" + userMsg
	}
	out, err := ai.ChatJSONWithRetry(ctx, llm, cb, tableGenSystemPrompt(), userMsg,
		ai.ChatOptions{Temperature: 0.2, MaxTokens: 1500},
		func(s string) bool { _, e := parseGeneratedTable(s); return e == nil })
	if err != nil {
		helpers.LogErrorWithContext(ctx, "GenerateTable chat err: %v", err)
		return nil, fmt.Errorf("could not generate the table")
	}

	gen, perr := parseGeneratedTable(out)
	if perr != nil {
		helpers.LogErrorWithContext(ctx, "GenerateTable parse err: %v (raw=%.300s)", perr, out)
		return nil, fmt.Errorf("could not understand that; try describing it differently")
	}

	// Build the table input and typed columns.
	tableIn := TableInput{
		Name:        gen.Name,
		Description: gen.Description,
		Icon:        gen.Icon,
		Visibility:  model.VisibilityWorkspace,
	}
	fields := make([]FieldInput, 0, len(gen.Fields))
	for i, f := range gen.Fields {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			continue
		}
		ftype := strings.TrimSpace(f.Type)
		if !model.ValidFieldType(ftype) {
			ftype = model.FieldText
		}
		cfg := map[string]interface{}{}
		if (ftype == model.FieldSelect || ftype == model.FieldMultiSelect) && len(f.Options) > 0 {
			opts := make([]map[string]interface{}, 0, len(f.Options))
			for _, o := range f.Options {
				if o = strings.TrimSpace(o); o != "" {
					opts = append(opts, map[string]interface{}{"label": o})
				}
			}
			cfg["options"] = opts
		}
		fields = append(fields, FieldInput{Name: name, Type: ftype, Config: cfg, Position: float64(i)})
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("could not derive any columns; try describing it differently")
	}

	tbl, err := CreateTableFromTemplate(ctx, tableIn, fields, nil, actor)
	if err != nil {
		return nil, err
	}

	// Map column name -> field id (case-insensitive) so generated rows, which
	// are keyed by column name, can be written by id.
	created, lerr := model.ListFields(ctx, tbl.Id)
	if lerr == nil && len(gen.Rows) > 0 {
		byName := make(map[string]string, len(created))
		for _, cf := range created {
			byName[strings.ToLower(strings.TrimSpace(cf.Name))] = cf.Id.String()
		}
		rowCount := 0
		for _, raw := range gen.Rows {
			if rowCount >= maxGeneratedRows {
				break
			}
			values := map[string]interface{}{}
			for k, v := range raw {
				if fid, ok := byName[strings.ToLower(strings.TrimSpace(k))]; ok {
					values[fid] = v
				}
			}
			if len(values) == 0 {
				continue
			}
			if _, rerr := CreateRow(ctx, tbl.Id, RowInput{Values: values, Position: float64(rowCount)}, actor); rerr != nil {
				helpers.LogErrorWithContext(ctx, "GenerateTable seed row err: %v", rerr)
				continue
			}
			rowCount++
		}
	}
	return tbl, nil
}

func parseGeneratedTable(raw string) (*generatedTable, error) {
	js := extractJSONObjectFrom(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var g generatedTable
	if err := json.Unmarshal([]byte(js), &g); err != nil {
		return nil, err
	}
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		g.Name = "Untitled table"
	}
	if len(g.Name) > maxNameLen {
		g.Name = g.Name[:maxNameLen]
	}
	return &g, nil
}

func tableGenSystemPrompt() string {
	return strings.Join([]string{
		"You design a structured data table from a plain-English description. Output ONLY a single JSON object, no prose.",
		"",
		"Schema:",
		"{",
		"  \"name\": short table title,",
		"  \"icon\": a single emoji that fits the table,",
		"  \"description\": one short sentence,",
		"  \"fields\": array of columns, each {\"name\": string, \"type\": one of text|number|select|multi_select|date|checkbox|url|email|person, \"options\": array of strings (ONLY for select/multi_select)},",
		"  \"rows\": array of up to 8 example rows; each row is an object keyed by COLUMN NAME with a sensible value",
		"}",
		"",
		"RULES:",
		"1. The first column should be a human-readable 'Name'/'Title' text column.",
		"2. Pick 3-7 useful columns with the most appropriate types. Use select/multi_select (with options) for categorical fields, date for dates (YYYY-MM-DD), checkbox for yes/no, number for quantities.",
		"3. Row values must match their column type: number -> a number, checkbox -> true/false, date -> \"YYYY-MM-DD\", select -> one of its options, multi_select -> array of its options.",
		"4. If workspace context is provided and the description refers to existing work (e.g. 'action items from #engineering', 'leads we discussed'), derive the seed rows from that real content. If the context is not relevant, ignore it and use plausible examples.",
		"5. Do NOT invent ids or reference external systems. Output a single valid JSON object and nothing else.",
	}, "\n")
}

// extractJSONObjectFrom is a local copy of the tolerant JSON-object extractor
// (the workflow package has its own; we avoid a cross-package dependency).
func extractJSONObjectFrom(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
