//go:build integration

package integration

import (
	"strings"
	"testing"
)

// fkUnindexedAllowed are foreign keys left without an index, by
// "table.column", and why.
var fkUnindexedAllowed = map[string]string{
	"ai_settings.embedding_provider_id":    "ai_settings is one row",
	"ai_settings.chat_provider_id":         "ai_settings is one row",
	"ai_settings.code_pr_chat_provider_id": "ai_settings is one row",
}

// Every single-column foreign key has an index that leads with it, once every
// migration has run. A partial index on just "column IS NOT NULL" counts: a
// delete looks for rows that point at it with column = $1, which implies it.
//
// WHY. Deleting a user, channel, project or team (all four are hard-deleted)
// makes Postgres look for the rows that point at it in every table with a
// foreign key to it: 99 of those columns had no index, so each delete scanned
// 99 tables whole, posts and chats among them, inside the deleting
// transaction. A new foreign key ships with its index (a CONCURRENTLY
// migration), or a reason in fkUnindexedAllowed.
func TestEveryForeignKeyHasAnIndex(t *testing.T) {
	env := SetupEnv(t)
	rows, err := env.PG.Query(`
		SELECT c.conrelid::regclass::text, a.attname
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND array_length(c.conkey, 1) = 1
		  AND NOT EXISTS (
			SELECT 1 FROM pg_index i
			WHERE i.indrelid = c.conrelid AND i.indkey[0] = c.conkey[1] AND i.indisvalid
			  AND (i.indpred IS NULL OR pg_get_expr(i.indpred, i.indrelid) = '(' || quote_ident(a.attname) || ' IS NOT NULL)'))
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		key := strings.TrimPrefix(table, "public.") + "." + col
		seen[key] = true
		if _, ok := fkUnindexedAllowed[key]; !ok {
			t.Errorf("foreign key %s has no index leading with it; add a CONCURRENTLY index migration", key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for key := range fkUnindexedAllowed {
		if !seen[key] {
			t.Errorf("%s is allowed unindexed but is indexed or gone now; take it off fkUnindexedAllowed", key)
		}
	}
}
