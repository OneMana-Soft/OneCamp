package helpers

// The migration lint (REPORT-2026-11-16 section 4; sketch B/lint-sketch.go).
//
// WHY. A migration runs while every server waits on it, once per install, and
// a shipped file can never change (13 Nov ruling: fix forward, no squash). So
// the rules are checked before a file ships, on the files above
// migrationLintFloor; files at or below it only keep their recorded bytes
// (L10), so no old file ever needs an edit. Raise the floor and rewrite the
// hashes (WRITE_MIGRATION_HASHES=1) at a release cut.
//
// How it differs from what exists: hotTableIndexGuard_test.go goes from
// queries to indexes (a hot read by an unindexed column); schema_test.go
// holds the embedded set to the directory; migrationParity_test.go holds the
// editions to the same bytes. This goes from migrations to locks, size and
// redundancy.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// migrationLintFloor is the highest version in the line when the lint landed
// (beta 86364bc48). Files above it are new and get the rules.
const migrationLintFloor = 604

// bigTables grow with every message, edit, event or log line: hotTables
// (hotTableIndexGuard_test.go) and the audit's tier-A tables. A new per-event
// table joins here.
var bigTables = append(append([]string{}, hotTables...),
	"comments", "thread_comments", "thread_comment_reactions", "attachments", "ai_chat_messages",
	"resource_mentions", "resource_mention_tracking", "resource_views", "resource_view_days",
	"notification_deliveries", "notification_email_log", "notification_email_queue", "outbox",
	"github_sync_queue", "github_webhook_deliveries", "webhook_deliveries", "webhook_logs",
	"admin_audit_log", "ai_agent_action_log", "ai_agent_runs", "csp_violations", "call_quality_samples",
	"data_table_rows", "data_table_cell_index", "data_table_changes", "data_table_computed",
	"data_table_computed_dirty", "data_table_links", "task_changes", "task_field_values", "import_id_map",
	"last_seen_channel", "last_seen_chat", "last_seen_activity", "users_chat_notification",
	"users_channel_notification", "slack_bridge_messages", "item_labels", "doc_snapshots",
	"calendar_events", "events", "event_attendees", "conversation_members")

// migrationLintExcused is "<version>:<rule>" -> why that file may break it.
var migrationLintExcused = map[string]string{
	"606:L5": "users.github_login is written only by CreateExternalGitHubUser, with a login GitHub gave (sync, webhook or import), and GitHub caps one at 39 characters; idx_users_github_login keyed the same text since 39, so the lower() key that replaces it adds no new limit",
}

// redundantIndexesHeld are the live indexes another index, primary key or
// unique constraint already covers, kept on purpose for now, by name -> why.
// It only shrinks: a new redundant index fails, and so does a line here whose
// index is gone (remove the line).
var redundantIndexesHeld = map[string]string{
	"idx_import_id_map_lookup":                  "big table (import_id_map, PK leads with it): a CONCURRENTLY drop waits for MAIN's idx_scan read on the demo",
	"idx_github_webhook_deliveries_delivery_id": "big table (github_webhook_deliveries, the UNIQUE on delivery_id): a CONCURRENTLY drop waits for MAIN's idx_scan read on the demo",
	"idx_webhook_logs_webhook_id":               "big table (webhook_logs, idx_webhook_logs_webhook_id_created leads with it): a CONCURRENTLY drop waits for MAIN's idx_scan read on the demo",
}

var (
	mlVersion     = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)
	mlCreateTable = regexp.MustCompile(`(?is)CREATE\s+(?:UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:public\.)?"?(\w+)"?\s*\((.*?)\n\)`)
	mlColumn      = regexp.MustCompile(`(?im)^\s*"?(\w+)"?\s+(text|citext|varchar(?:\s*\(\s*(\d+)\s*\))?|character\s+varying(?:\s*\(\s*(\d+)\s*\))?)\b`)
	mlInlineKey   = regexp.MustCompile(`(?im)^\s*"?(\w+)"?\s+[^,\n]*?\b(PRIMARY\s+KEY|UNIQUE)\b(\s*\()?`)
	mlTableKey    = regexp.MustCompile(`(?is)(?:CONSTRAINT\s+"?(\w+)"?\s+)?(PRIMARY\s+KEY|UNIQUE)\s*\(([^)]*)\)`)
	mlAlterTable  = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(?:public\.)?"?(\w+)"?\s+(.*?);`)
	mlAddColDef   = regexp.MustCompile(`(?is)ADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?\s+([^,;]*)`)
	mlAddKey      = regexp.MustCompile(`(?is)ADD\s+(?:CONSTRAINT\s+"?(\w+)"?\s+)?(PRIMARY\s+KEY|UNIQUE)\s*(?:\(([^)]*)\)|USING\s+INDEX\s+"?(\w+)"?)`)
	mlDropKey     = regexp.MustCompile(`(?is)DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?`)
	mlRenameTable = regexp.MustCompile(`(?is)^RENAME\s+TO\s+"?(\w+)"?`)
	mlRenameIndex = regexp.MustCompile(`(?is)ALTER\s+INDEX\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?\s+RENAME\s+TO\s+"?(\w+)"?`)
	mlDropTable   = regexp.MustCompile(`(?is)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([\w\s,."]+?)\s*(?:CASCADE|RESTRICT)?\s*;`)
	mlIndex       = regexp.MustCompile(`(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?\s+ON\s+(?:ONLY\s+)?(?:public\.)?"?(\w+)"?\s*(?:USING\s+(\w+)\s*)?\((.*?)\)\s*(?:INCLUDE\s*\([^)]*\)\s*)?(?:WHERE\s+([^;]*))?;`)
	mlDropIndex   = regexp.MustCompile(`(?is)DROP\s+INDEX\s+(CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(?:public\.)?"?(\w+)"?`)
	mlConcurrent  = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)
	mlAlterType   = regexp.MustCompile(`(?is)ALTER\s+(?:COLUMN\s+)?"?\w+"?\s+(?:SET\s+DATA\s+)?TYPE\b`)
	mlVolatile    = regexp.MustCompile(`(?is)\b(serial|bigserial|smallserial)\b|DEFAULT\s+(gen_random_uuid|uuid_generate_v4|clock_timestamp|random|nextval|timeofday)\s*\(|GENERATED\s+ALWAYS\s+AS\s*\(.*\)\s*STORED`)
	mlConstraint  = regexp.MustCompile(`(?is)ADD\s+(?:CONSTRAINT\s+\w+\s+)?(CHECK|FOREIGN\s+KEY|UNIQUE|PRIMARY\s+KEY)\b([^;]*)`)
	mlDML         = regexp.MustCompile(`(?is)(?:^|;)\s*(?:UPDATE|DELETE\s+FROM)\s+(?:ONLY\s+)?(?:public\.)?"?(\w+)"?`)
	mlComment     = regexp.MustCompile(`(?m)--[^\n]*$`)
)

// mlIdx is a live index: one a CREATE INDEX made (key false) or the one a
// primary key or unique constraint owns (key true).
type mlIdx struct {
	name, table, method, pred string
	keys                      []string
	unique, key               bool
	version                   int
}

// mlFile is one version's pair: sql is the up without comments, raw its
// bytes, down the down without comments ("" when there is none).
type mlFile struct {
	version         int
	name, sql, down string
	raw             []byte
	hasDown         bool
}

func mlRead(t *testing.T, dir string) []mlFile {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []mlFile
	for _, e := range ents {
		m := mlVersion.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		f := mlFile{version: v, name: e.Name(), sql: mlComment.ReplaceAllString(string(b), ""), raw: b}
		if d, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(e.Name(), ".up.sql")+".down.sql")); err == nil {
			f.down, f.hasDown = mlComment.ReplaceAllString(string(d), ""), true
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

func mlNorm(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = strings.ReplaceAll(s, `"`, "")
	return strings.TrimSuffix(strings.TrimPrefix(s, "("), ")")
}

func mlKeys(inner string) []string {
	var keys []string
	depth, cur := 0, ""
	for _, r := range inner {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			keys = append(keys, strings.TrimSuffix(mlNorm(cur), " asc"))
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(keys, strings.TrimSuffix(mlNorm(cur), " asc"))
}

func isBig(t string) bool {
	for _, b := range bigTables {
		if b == t {
			return true
		}
	}
	return false
}

// mlKeyIndex is the index a primary key or unique constraint owns, named as
// Postgres names it when the file doesn't.
func mlKeyIndex(table, name, kind, cols string, version int) mlIdx {
	keys := mlKeys(cols)
	if name == "" {
		if strings.HasPrefix(strings.ToUpper(kind), "PRIMARY") {
			name = table + "_pkey"
		} else {
			name = table + "_" + strings.Join(keys, "_") + "_key"
		}
	}
	return mlIdx{name: strings.ToLower(name), table: table, method: "btree", keys: keys, unique: true, key: true, version: version}
}

// mlLint replays files in order (those at or below floor only to learn the
// schema and the live indexes) and returns what the files above floor break,
// as "<file>: <rule>: <why>", with the indexes live at the end.
func mlLint(files []mlFile, floor int, excused map[string]string) ([]string, map[string]mlIdx) {
	var out []string
	textCols := map[string]bool{} // "table.col" -> unbounded text
	live := map[string]mlIdx{}
	createdIn := map[string]int{}
	fail := func(f mlFile, rule, msg string) {
		if _, ok := excused[strconv.Itoa(f.version)+":"+rule]; !ok {
			out = append(out, f.name+": "+rule+": "+msg)
		}
	}
	touched := map[string][]string{} // table -> new transactional files that ALTER it
	for _, f := range files {
		isNew := f.version > floor
		noTx := strings.HasPrefix(string(f.raw), "-- no-transaction\n")
		if isNew && mlConcurrent.MatchString(f.sql) && (!noTx || strings.Count(strings.TrimSpace(f.sql), ";") > 1) {
			fail(f, "L1", "CONCURRENTLY can't run in a transaction: '-- no-transaction' as the first line and one statement in the file")
		}
		for _, m := range mlCreateTable.FindAllStringSubmatch(f.sql, -1) {
			tb := strings.ToLower(m[1])
			createdIn[tb] = f.version
			for _, c := range mlColumn.FindAllStringSubmatch(m[2], -1) {
				n, _ := strconv.Atoi(c[3] + c[4])
				textCols[tb+"."+strings.ToLower(c[1])] = n == 0 || n > 676
			}
			for _, k := range mlInlineKey.FindAllStringSubmatch(m[2], -1) {
				if c := strings.ToUpper(k[1]); k[3] == "" && c != "CONSTRAINT" && c != "PRIMARY" && c != "UNIQUE" {
					ix := mlKeyIndex(tb, "", k[2], k[1], f.version)
					live[ix.name] = ix
				}
			}
			for _, k := range mlTableKey.FindAllStringSubmatch(m[2], -1) {
				ix := mlKeyIndex(tb, k[1], k[2], k[3], f.version)
				live[ix.name] = ix
			}
		}
		for _, m := range mlAlterTable.FindAllStringSubmatch(f.sql, -1) {
			tb := strings.ToLower(m[1])
			if r := mlRenameTable.FindStringSubmatch(strings.TrimSpace(m[2])); r != nil {
				for n, ix := range live {
					if ix.table == tb {
						ix.table = strings.ToLower(r[1])
						live[n] = ix
					}
				}
				continue
			}
			for _, k := range mlDropKey.FindAllStringSubmatch(m[2], -1) {
				delete(live, strings.ToLower(k[1]))
			}
			for _, k := range mlAddKey.FindAllStringSubmatch(m[2], -1) {
				if k[4] != "" { // USING INDEX: the index becomes the constraint's
					if ix, ok := live[strings.ToLower(k[4])]; ok {
						delete(live, ix.name)
						ix.key, ix.unique = true, true
						if k[1] != "" {
							ix.name = strings.ToLower(k[1])
						}
						live[ix.name] = ix
					}
					continue
				}
				ix := mlKeyIndex(tb, k[1], k[2], k[3], f.version)
				live[ix.name] = ix
			}
			for _, c := range mlAddColDef.FindAllStringSubmatch(m[2], -1) {
				if cc := mlColumn.FindStringSubmatch(c[1] + " " + c[2]); cc != nil {
					n, _ := strconv.Atoi(cc[3] + cc[4])
					textCols[tb+"."+strings.ToLower(c[1])] = n == 0 || n > 676
				}
				if isNew && isBig(tb) && createdIn[tb] != f.version && mlVolatile.MatchString(c[2]) {
					fail(f, "L3", "ADD COLUMN "+c[1]+" on big table "+tb+" with a volatile default or serial rewrites every row; add it nullable, backfill in batches from the server, then set the default")
				}
			}
			if !isNew || createdIn[tb] == f.version {
				continue
			}
			if isBig(tb) && mlAlterType.MatchString(m[2]) {
				fail(f, "L2", "ALTER COLUMN TYPE on big table "+tb+" rewrites it under an exclusive lock; add a new column and switch reads instead")
			}
			if c := mlConstraint.FindStringSubmatch(m[2]); c != nil && isBig(tb) {
				kind := strings.ToUpper(strings.Fields(c[1])[0])
				if (kind == "CHECK" || kind == "FOREIGN") && !strings.Contains(strings.ToUpper(c[2]), "NOT VALID") {
					fail(f, "L8", kind+" on big table "+tb+" scans it under ACCESS EXCLUSIVE; add it NOT VALID and VALIDATE CONSTRAINT in a later file")
				}
				if (kind == "UNIQUE" || kind == "PRIMARY") && !strings.Contains(strings.ToUpper(c[2]), "USING INDEX") {
					fail(f, "L8", kind+" on big table "+tb+" builds its index under ACCESS EXCLUSIVE; build it CONCURRENTLY, then ADD CONSTRAINT ... USING INDEX")
				}
			}
			if !noTx {
				touched[tb] = append(touched[tb], f.name)
			}
		}
		for _, m := range mlRenameIndex.FindAllStringSubmatch(f.sql, -1) {
			if ix, ok := live[strings.ToLower(m[1])]; ok {
				delete(live, ix.name)
				ix.name = strings.ToLower(m[2])
				live[ix.name] = ix
			}
		}
		for _, m := range mlDropTable.FindAllStringSubmatch(f.sql, -1) {
			for _, tb := range strings.Split(m[1], ",") {
				tb = strings.ToLower(strings.Trim(strings.TrimSpace(tb), `"`))
				tb = strings.TrimPrefix(tb, "public.")
				for n, ix := range live {
					if ix.table == tb {
						delete(live, n)
					}
				}
			}
		}
		for _, m := range mlDropIndex.FindAllStringSubmatch(f.sql, -1) {
			name := strings.ToLower(m[2])
			if ix, ok := live[name]; ok && isNew && isBig(ix.table) && m[1] == "" {
				fail(f, "L1", "DROP INDEX "+name+" on big table "+ix.table+" without CONCURRENTLY blocks its reads and writes; one DROP INDEX CONCURRENTLY IF EXISTS per file, first line -- no-transaction")
			}
			delete(live, name)
		}
		for _, m := range mlIndex.FindAllStringSubmatch(f.sql, -1) {
			ix := mlIdx{name: strings.ToLower(m[3]), table: strings.ToLower(m[4]), method: strings.ToLower(m[5]), pred: mlNorm(m[7]), keys: mlKeys(m[6]), unique: m[1] != "", version: f.version}
			if ix.method == "" {
				ix.method = "btree"
			}
			if _, ok := live[ix.name]; ok && !isNew {
				continue // IF NOT EXISTS over one already there
			}
			if isNew {
				// L1: an index on a big table that existed before this file.
				if isBig(ix.table) && m[2] == "" && createdIn[ix.table] != f.version {
					fail(f, "L1", ix.name+" on big table "+ix.table+" without CONCURRENTLY blocks writes for the build; one CREATE INDEX CONCURRENTLY per file, first line -- no-transaction")
				}
				// L4: covered by an index that stays, or covering one.
				for _, o := range live {
					if o.name == ix.name || o.table != ix.table || o.method != ix.method || o.pred != ix.pred {
						continue
					}
					if mlPrefix(ix.keys, o.keys) && !(ix.unique && len(ix.keys) < len(o.keys)) {
						fail(f, "L4", ix.name+" is covered by "+o.name+" ("+strings.Join(o.keys, ",")+")")
					} else if mlPrefix(o.keys, ix.keys) && !o.unique {
						fail(f, "L4", ix.name+" makes "+o.name+" redundant; drop it in this head")
					}
				}
				// L5: btree key on unbounded text.
				if ix.method == "btree" {
					for _, k := range ix.keys {
						col := strings.TrimSuffix(strings.Fields(k)[0], ",")
						if strings.HasPrefix(k, "lower(") || strings.HasPrefix(k, "upper(") {
							col = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(k, "lower("), "upper("), ")")
						}
						bounded := strings.Contains(ix.pred, "octet_length("+col+")") || strings.HasPrefix(k, "left(") || strings.HasPrefix(k, "md5(")
						if textCols[ix.table+"."+col] && !bounded {
							fail(f, "L5", ix.name+" keys unbounded text "+ix.table+"."+col+"; Postgres refuses a btree entry over 2,704 bytes: key left(col,256)/md5(col), or add WHERE octet_length(col) <= N, or a CHECK on the column")
						}
					}
				}
			}
			live[ix.name] = ix
		}
		if isNew {
			for _, m := range mlDML.FindAllStringSubmatch(f.sql, -1) {
				if tb := strings.ToLower(m[1]); isBig(tb) && createdIn[tb] != f.version {
					fail(f, "L9", "UPDATE/DELETE over big table "+tb+" runs in the migration's transaction while every server waits; backfill in batches from the server, or excuse it with a reason")
				}
			}
			mlCheckDown(f, fail)
		}
	}
	// L6: one feature = one file pair per head (transactional files only:
	// CONCURRENTLY files are one statement each by necessity).
	for tb, fs := range touched {
		if len(fs) > 1 && mlSameFeature(fs) {
			out = append(out, fmt.Sprintf("L6: %v all ALTER %s: fold them into one file pair before the head lands", fs, tb))
		}
	}
	sort.Strings(out)
	return out, live
}

func mlPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mlSameFeature: the files' name stems share their first word (e.g. call_*, ai_*).
func mlSameFeature(fs []string) bool {
	first := func(n string) string { p := strings.SplitN(n, "_", 3); return p[1] }
	for _, f := range fs[1:] {
		if first(f) != first(fs[0]) {
			return false
		}
	}
	return true
}

// L7: the down exists, is not empty, and undoes what the up named: drops
// each table and index it created, recreates each index it dropped.
func mlCheckDown(f mlFile, fail func(mlFile, string, string)) {
	if !f.hasDown {
		fail(f, "L7", "no down file")
		return
	}
	down := strings.ToLower(f.down)
	if strings.TrimSpace(strings.ReplaceAll(down, ";", "")) == "" {
		fail(f, "L7", "empty down file")
		return
	}
	for _, m := range mlCreateTable.FindAllStringSubmatch(f.sql, -1) {
		if !regexp.MustCompile(`drop\s+table[^;]*\b` + strings.ToLower(m[1]) + `\b`).MatchString(down) {
			fail(f, "L7", "down does not drop table "+m[1])
		}
	}
	for _, m := range mlIndex.FindAllStringSubmatch(f.sql, -1) {
		if !strings.Contains(down, strings.ToLower(m[3])) && !strings.Contains(down, "drop table") {
			fail(f, "L7", "down does not drop index "+m[3])
		}
	}
	for _, m := range mlDropIndex.FindAllStringSubmatch(f.sql, -1) {
		if !regexp.MustCompile(`create\s+(unique\s+)?index\s+(if\s+not\s+exists\s+)?"?` + strings.ToLower(m[2]) + `\b`).MatchString(down) {
			fail(f, "L7", "down does not recreate index "+m[2]+" (copy its definition from the migration that made it)")
		}
	}
}

// mlRedundant is each live index another live index, primary key or unique
// constraint already covers (same table, method and predicate, its keys a
// leading part of the other's), by name -> the one that covers it.
func mlRedundant(live map[string]mlIdx) map[string]string {
	out := map[string]string{}
	for _, ix := range live {
		if ix.key {
			continue
		}
		for _, o := range live {
			if o.name == ix.name || o.table != ix.table || o.method != ix.method || o.pred != ix.pred || !mlPrefix(ix.keys, o.keys) {
				continue
			}
			if len(ix.keys) == len(o.keys) {
				// Identical: the constraint's, the unique one, or the older stays.
				if !o.key && (ix.unique && !o.unique || ix.unique == o.unique && (ix.version < o.version || ix.version == o.version && ix.name < o.name)) {
					continue
				}
			} else if ix.unique {
				continue
			}
			out[ix.name] = o.name
		}
	}
	return out
}

// New files follow the rules.
func TestNewMigrationsFollowTheRules(t *testing.T) {
	found, _ := mlLint(mlRead(t, filepath.Join("..", "migrations")), migrationLintFloor, migrationLintExcused)
	for _, f := range found {
		t.Error(f)
	}
}

// Redundant indexes only get fewer (rule L4 over the whole schema; the audit
// counted 10).
func TestRedundantIndexesOnlyGetFewer(t *testing.T) {
	files := mlRead(t, filepath.Join("..", "migrations"))
	if len(files) < 450 {
		t.Fatalf("read %d migrations; the lint is no longer looking at them", len(files))
	}
	_, live := mlLint(files, migrationLintFloor, migrationLintExcused)
	red := mlRedundant(live)
	var names []string
	for n := range red {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := redundantIndexesHeld[n]; !ok {
			t.Errorf("%s on %s is covered by %s: drop it (fix forward; CONCURRENTLY in its own file on a big table)", n, live[n].table, red[n])
		}
	}
	for n := range redundantIndexesHeld {
		if _, ok := red[n]; !ok {
			t.Errorf("%s is no longer a redundant live index: remove it from redundantIndexesHeld", n)
		}
	}
}

// The lint catches each rule's fixture: replayed with floor 1, so version 1
// is the shipped schema and the rest are new files.
func TestMigrationLintCatchesTheFixtures(t *testing.T) {
	base := mlFile{version: 1, name: "1_base.up.sql", hasDown: true, down: "DROP TABLE tasks; DROP TABLE notes;",
		sql: "CREATE TABLE tasks (\n id uuid PRIMARY KEY,\n project_id uuid,\n title text\n);\nCREATE TABLE notes (\n id uuid PRIMARY KEY,\n body text\n);\nCREATE INDEX idx_tasks_project ON tasks (project_id);\nCREATE INDEX idx_notes_body_len ON notes (length(body));\n"}
	cases := []struct {
		name, rule string
		f          mlFile
	}{
		{"an index on tasks without CONCURRENTLY", "L1", mlFile{sql: "CREATE INDEX idx_tasks_title_len ON tasks (length(title));", down: "DROP INDEX idx_tasks_title_len;", hasDown: true}},
		{"a drop on tasks without CONCURRENTLY", "L1", mlFile{sql: "DROP INDEX IF EXISTS idx_tasks_project;", down: "CREATE INDEX idx_tasks_project ON tasks (project_id);", hasDown: true}},
		{"CONCURRENTLY inside a transaction", "L1", mlFile{sql: "CREATE INDEX CONCURRENTLY idx_tasks_title_len ON tasks (length(title));", down: "DROP INDEX idx_tasks_title_len;", hasDown: true}},
		{"two CONCURRENTLY statements in one file", "L1", mlFile{raw: []byte("-- no-transaction\n"), sql: "DROP INDEX CONCURRENTLY IF EXISTS idx_tasks_project;\nCREATE INDEX CONCURRENTLY idx_tasks_title_len ON tasks (length(title));", down: "CREATE INDEX idx_tasks_project ON tasks (project_id); DROP INDEX idx_tasks_title_len;", hasDown: true}},
		{"an index the primary key covers", "L4", mlFile{sql: "CREATE INDEX idx_notes_id ON notes (id);", down: "DROP INDEX idx_notes_id;", hasDown: true}},
		{"a down that does not restore its index", "L7", mlFile{sql: "DROP INDEX IF EXISTS idx_notes_body_len;", down: "SELECT 1;", hasDown: true}},
		{"no down file", "L7", mlFile{sql: "CREATE INDEX idx_notes_body_len2 ON notes (length(body));"}},
	}
	for _, c := range cases {
		c.f.version, c.f.name = 2, "2_fixture.up.sql"
		if c.f.raw == nil {
			c.f.raw = []byte(c.f.sql)
		}
		found, _ := mlLint([]mlFile{base, c.f}, 1, nil)
		if !strings.Contains(strings.Join(found, "\n"), ": "+c.rule+": ") {
			t.Errorf("%s: the lint found %q, want a %s finding", c.name, found, c.rule)
		}
	}
	// And a clean pair passes: a CONCURRENTLY drop on its own whose down
	// recreates the index.
	ok := mlFile{version: 2, name: "2_ok.up.sql", raw: []byte("-- no-transaction\nDROP INDEX CONCURRENTLY IF EXISTS idx_tasks_project;\n"),
		sql: "\nDROP INDEX CONCURRENTLY IF EXISTS idx_tasks_project;\n", down: "CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks (project_id);", hasDown: true}
	if found, _ := mlLint([]mlFile{base, ok}, 1, nil); len(found) > 0 {
		t.Errorf("a clean CONCURRENTLY drop: the lint found %q", found)
	}
}

// L10: files at or below the floor keep the bytes they shipped with.
// testdata/migration-hashes.txt is "<sha256>  <file>" per line, written at a
// release cut with WRITE_MIGRATION_HASHES=1, never by hand.
func TestShippedMigrationsKeepTheirBytes(t *testing.T) {
	dir := filepath.Join("..", "migrations")
	hashes := filepath.Join("testdata", "migration-hashes.txt")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]string{}
	pair := regexp.MustCompile(`^(\d+)_.*\.(up|down)\.sql$`)
	for _, e := range ents {
		m := pair.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, _ := strconv.Atoi(m[1]); v > migrationLintFloor {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		have[e.Name()] = hex.EncodeToString(sum[:])
	}
	if os.Getenv("WRITE_MIGRATION_HASHES") != "" {
		var lines []string
		for n, h := range have {
			lines = append(lines, h+"  "+n)
		}
		sort.Slice(lines, func(i, j int) bool { return lines[i][66:] < lines[j][66:] })
		if err := os.WriteFile(hashes, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := map[string]string{}
	fh, err := os.Open(hashes)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if p := strings.Fields(sc.Text()); len(p) == 2 {
			want[p[1]] = p[0]
		}
	}
	if len(want) < 900 {
		t.Fatalf("%s holds %d hashes; it should hold every shipped file", hashes, len(want))
	}
	for n, h := range have {
		if w, ok := want[n]; !ok {
			t.Errorf("%s is at or below the floor but has no recorded hash: a shipped number was reused or renamed", n)
		} else if w != h {
			t.Errorf("%s changed after it shipped: a server that ran it refuses to start (sqlx checksum); fix forward in a new file", n)
		}
	}
	for n := range want {
		if _, ok := have[n]; !ok {
			t.Errorf("%s shipped and is gone: sqlx refuses a database that ran a migration the build lacks", n)
		}
	}
}
