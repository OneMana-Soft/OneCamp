package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hotTables are the tables every channel list, conversation, board or table
// edit reads; a query on one by a column no index leads with scans it.
var hotTables = []string{"posts", "chats", "tasks", "data_table_change_sets"}

// hotTableUnindexed is what reads one of hotTables by a column no index leads
// with today, by "table.column", and why that is acceptable. Empty: a new one
// needs an index (a CONCURRENTLY migration) or a reason here.
var hotTableUnindexed = map[string]string{
	// A task by its key (task keys): always with its project, and
	// tasks_project_number (project_id, task_number) leads with project_id.
	"tasks.task_number": "read with project_id = …, which tasks_project_number (project_id, task_number) leads with",
}

var (
	createIndex = regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s+(?:public\.)?(\w+)\s*(?:USING\s+\w+\s*)?\(\s*"?(\w+)`)
	dropIndex   = regexp.MustCompile(`(?is)DROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(\w+)`)
)

// leadingIndexColumns is, per table, the columns some index leads with once
// every migration has run in order (the primary key's id included).
func leadingIndexColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	version := func(f string) int {
		n, _ := strconv.Atoi(strings.SplitN(filepath.Base(f), "_", 2)[0])
		return n
	}
	sort.Slice(files, func(i, j int) bool { return version(files[i]) < version(files[j]) })
	type idx struct{ table, col string }
	byName := map[string]idx{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(string(b), ";") {
			if m := createIndex.FindStringSubmatch(stmt); m != nil {
				byName[strings.ToLower(m[1])] = idx{strings.ToLower(m[2]), strings.ToLower(m[3])}
			} else if m := dropIndex.FindStringSubmatch(stmt); m != nil {
				delete(byName, strings.ToLower(m[1]))
			}
		}
	}
	out := map[string]map[string]bool{}
	for _, tb := range hotTables {
		out[tb] = map[string]bool{"id": true}
	}
	for _, i := range byName {
		if out[i.table] != nil {
			out[i.table][i.col] = true
		}
	}
	return out
}

// Every query that picks rows of posts, chats, tasks or
// data_table_change_sets by a column picks them by one an index leads with.
//
// WHY. posts had no index on post_channel: the unread count every channel list
// and user load runs (domain/Post, domain/LastSeenChannel) scanned all posts.
// chats had grp_id alone for reads by conversation and time, and the change
// sets' undoes and undone_by, both self-references set to NULL by the
// retention delete, had none, so that delete was quadratic. This reads every
// query's first WHERE column (FROM t WHERE c, DELETE FROM t WHERE c, UPDATE t
// SET ... WHERE c) and join column (JOIN t a ON a.c), and the indexes the
// migrations leave, and fails on a column no index leads with that
// hotTableUnindexed doesn't excuse.
func TestHotTablesAreReadByIndexedColumns(t *testing.T) {
	lead := leadingIndexColumns(t)
	tables := strings.Join(hotTables, "|")
	shapes := []*regexp.Regexp{
		regexp.MustCompile(`(?is)\bFROM\s+(` + tables + `)\b(?:\s+(?:AS\s+)?(\w+))?\s+WHERE\s+\(?\s*(?:(\w+)\.)?(\w+)`),
		regexp.MustCompile(`(?is)\bJOIN\s+(` + tables + `)\b(?:\s+(?:AS\s+)?(\w+))?\s+ON\s+\(?\s*(?:(\w+)\.)?(\w+)`),
		regexp.MustCompile(`(?is)\bUPDATE\s+(` + tables + `)\b(?:\s+(\w+))?\s+SET\s+(?:[^;` + "`" + `]*?)\bWHERE\s+\(?\s*(?:(\w+)\.)?(\w+)`),
	}
	reserved := map[string]bool{"where": true, "set": true, "on": true, "left": true, "join": true, "inner": true, "order": true, "group": true, "limit": true}
	found := map[string][]string{}
	read := 0
	for _, root := range []string{"../business", "../models", "../domain", "../controllers", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			read++
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, rx := range shapes {
				for _, m := range rx.FindAllStringSubmatch(string(b), -1) {
					table, alias, qual, col := strings.ToLower(m[1]), strings.ToLower(m[2]), strings.ToLower(m[3]), strings.ToLower(m[4])
					if reserved[alias] {
						alias = ""
					}
					// A column of another table (the join's other side) says nothing.
					if qual != "" && qual != alias && qual != table {
						continue
					}
					if lead[table][col] {
						continue
					}
					key := table + "." + col
					found[key] = append(found[key], path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if read < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the queries", read)
	}
	for key, where := range found {
		if _, ok := hotTableUnindexed[key]; !ok {
			t.Errorf("%s is read by a column no index leads with (%s); add a CONCURRENTLY index migration, or say why in hotTableUnindexed", key, strings.Join(where, ", "))
		}
	}
	for key := range hotTableUnindexed {
		if _, ok := found[key]; !ok {
			t.Errorf("%s is excused in hotTableUnindexed but nothing reads it that way now; take it off", key)
		}
	}
}
