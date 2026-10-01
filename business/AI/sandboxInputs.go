package business

// sandboxInputs.go — pure bridge from OneCamp table data to a sandbox InputFile.
//
// The run_analysis executor resolves an input binding (a table read or an
// aggregate) to concrete, permission-checked data, then uses these helpers to
// serialize it to a CSV the sandbox exposes to the code (see codesandbox
// PrepareInputFiles / BuildPythonPreamble). Kept pure (already-fetched data in,
// bytes out) so the data→file shape is unit-testable without a DB or a runner.

import (
	"encoding/json"
	"strconv"
	"strings"

	codesandbox "github.com/akashc777/OneCamp/business/CodeSandbox"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dtmodel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// tableRowsToInputFile serializes a table's fields + rows to a CSV InputFile:
// header = field names (in the given field order), one CSV row per data row,
// each cell stringified from the row's JSON values. Deterministic. rows beyond
// maxRows are dropped (the caller also bounds the fetch) so a run can't ingest
// an unbounded dataset.
func tableRowsToInputFile(name string, fields []*dtmodel.Field, rows []*dtmodel.Row, maxRows int) codesandbox.InputFile {
	header := make([]string, 0, len(fields))
	fieldIDs := make([]string, 0, len(fields))
	for _, f := range fields {
		header = append(header, f.Name)
		fieldIDs = append(fieldIDs, f.Id.String())
	}

	out := make([][]string, 0, len(rows))
	for i, r := range rows {
		if maxRows > 0 && i >= maxRows {
			break
		}
		values := parseSandboxRowValues(r.Values)
		rec := make([]string, len(fieldIDs))
		for j, fid := range fieldIDs {
			rec[j] = stringifyCell(values[fid])
		}
		out = append(out, rec)
	}
	return codesandbox.InputFile{
		Name:   name,
		Format: codesandbox.FormatCSV,
		Bytes:  codesandbox.EncodeCSV(header, out),
	}
}

// aggResultToInputFile serializes an aggregation into a CSV InputFile with a
// stable schema: group, value, count. The group column header uses the
// aggregation's group-by label when present.
func aggResultToInputFile(name string, res *dataTableBusiness.AggResult) codesandbox.InputFile {
	groupCol := "group"
	if res != nil && strings.TrimSpace(res.GroupByLabel) != "" {
		groupCol = res.GroupByLabel
	}
	valueCol := "value"
	if res != nil && strings.TrimSpace(res.ValueFieldLabel) != "" {
		valueCol = string(res.Op) + "_" + res.ValueFieldLabel
	} else if res != nil {
		valueCol = string(res.Op)
	}
	header := []string{groupCol, valueCol, "count"}
	var rows [][]string
	if res != nil {
		rows = make([][]string, 0, len(res.Buckets))
		for _, b := range res.Buckets {
			rows = append(rows, []string{b.Label, trimFloat(b.Value), strconv.Itoa(b.Count)})
		}
	}
	return codesandbox.InputFile{
		Name:   name,
		Format: codesandbox.FormatCSV,
		Bytes:  codesandbox.EncodeCSV(header, rows),
	}
}

// parseSandboxRowValues decodes a row's JSON values blob; never errors (a bad
// blob yields an empty map so the row contributes empty cells).
func parseSandboxRowValues(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}

// stringifyCell renders a JSON cell value as a flat CSV cell: strings as-is,
// numbers trimmed, bools as true/false, arrays/objects joined/JSON-encoded so a
// relation/multi-select cell becomes a readable value rather than Go's %v.
func stringifyCell(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return trimFloat(t)
	case json.Number:
		return t.String()
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, stringifyCell(e))
		}
		return strings.Join(parts, "; ")
	case map[string]interface{}:
		// A relation/person ref: prefer a human label, else compact JSON.
		for _, k := range []string{"label", "name", "value", "title", "id"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		b, _ := json.Marshal(t)
		return string(b)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// trimFloat formats a float without trailing zeros.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
