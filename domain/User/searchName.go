package domain

// Where people searches start.
//
// SearchPeople and GetUserListWithSearchText match a person's name, full
// name (and, for SearchPeople, job title and department) with a regexp. Run
// from has(user_uuid), that reads every person on every keystroke. A trigram
// index on user_name would let the search start from the matches, but
// user_name is @upsert, and Dgraph makes every key of an @upsert predicate's
// indexes a conflict key: a trigram index on it made people joining at once
// with like names abort each other's sign-up (c925fc70).
//
// So each person also holds user_search_name, without @upsert, with a trigram
// index: SearchName of their four names. Its trigrams are never conflict
// keys. A search whose term is long enough for the index starts from the
// people whose search name matches (dgraphquery.RegexpUnionRoot) and keeps
// its full match as the filter, so what comes back is the same as from every
// person: anything that matches one of the names matches the value that holds
// them all.
//
// It is written in one place, CreateOrUpdateDgraphUser, which every writer of
// a person's names goes through (sign-up, profile, handles, SCIM and SSO
// joins, bots and agents, the demo's people), in the same transaction as the
// names and from the names as they will be stored. People saved before it
// existed get theirs from BackfillSearchNames at start.

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// SearchName is a person's user_search_name: their name, full name, job title
// and department, lower-cased, joined by a space, the empty ones left out.
//
// Lower-cased, so the index holds one form of each trigram. The searches
// still match /…/i, so the case of what is typed doesn't matter either way. A
// letter whose lower case /i wouldn't match against it (the dotted capital
// İ lowers to i, which /i never pairs with İ) is kept as written, so a term
// the filter matches in a name always matches here too.
func SearchName(name, fullName, jobTitle, department string) string {
	var parts []string
	for _, s := range []string{name, fullName, jobTitle, department} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, lowerKeepingFolds(s))
		}
	}
	return strings.Join(parts, " ")
}

// lowerKeepingFolds lower-cases each letter of s whose lower case is one of
// its case-insensitive matches, and keeps the others.
func lowerKeepingFolds(s string) string {
	return strings.Map(func(r rune) rune {
		l := unicode.ToLower(r)
		if l == r {
			return r
		}
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f == l {
				return l
			}
		}
		return r
	}, s)
}

// searchNameOf is SearchName of what a write leaves stored: each name the
// write carries, else the one stored (a write leaves out what it doesn't
// change).
func searchNameOf(update, stored *dgraphStruct.DgraphUser) string {
	pick := func(u, s string) string {
		if u != "" {
			return u
		}
		return s
	}
	return SearchName(pick(update.UserName, stored.UserName), pick(update.UserFullName, stored.UserFullName),
		pick(update.Title, stored.Title), pick(update.Department, stored.Department))
}

// storedNamesQuery reads the names a person holds, by $id (their uuid).
const storedNamesQuery = `query names($id: string) {
	people(func: eq(user_uuid, $id)) { user_name user_full_name user_job_title user_department }
}`

// people is a query's people block.
type people struct {
	People []dgraphStruct.DgraphUser `json:"people"`
}

// searchNameBackfillPage is how many people BackfillSearchNames reads and
// writes in one transaction.
const searchNameBackfillPage = 200

// BackfillSearchNames gives everyone whose user_search_name isn't what their
// names make (people saved before it existed) the right one, a page at a time
// in uid order, and answers how many it wrote. Each page is read and written
// in one transaction, so a profile saved meanwhile either lands first and is
// read, or aborts the page, which is read again. When everyone's is right, a
// run reads every person once and writes nothing.
func BackfillSearchNames(ctx context.Context) (written int, err error) {
	after := ""
	for {
		var n int
		var last string
		err = dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
			n, last = 0, ""
			from := ""
			if after != "" {
				from = ", after: " + after
			}
			got, err := dgexec.ReadIn[people](tx, fmt.Sprintf(`{
				people(func: has(user_uuid), first: %d%s) { uid user_name user_full_name user_job_title user_department user_search_name }
			}`, searchNameBackfillPage, from), nil)
			if err != nil {
				return err
			}
			var sets []dgraphStruct.DgraphUser
			for _, p := range got.People {
				last = p.Uid
				if want := SearchName(p.UserName, p.UserFullName, p.Title, p.Department); want != "" && want != p.SearchName {
					sets = append(sets, dgraphStruct.DgraphUser{Uid: p.Uid, SearchName: want})
				}
			}
			n = len(sets)
			if len(got.People) == 0 || len(sets) == 0 {
				return nil
			}
			_, err = tx.Mutate("", nil, dgexec.Mutation{Set: sets})
			return err
		})
		if err != nil {
			return written, err
		}
		written += n
		if last == "" {
			return written, nil
		}
		after = last
	}
}
