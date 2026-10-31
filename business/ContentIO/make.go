package business

import (
	"bytes"
	"context"
	"io"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// Made is what MakePages made, or a Targeted importer (Created and Skipped).
type Made struct {
	// Created are the docs there are now, parents first.
	Created []Created
	// Skipped is what was left out, and why.
	Skipped []Skipped
	// Filled is how many docs got their content.
	Filled int
	// Docs is the doc each page became, by the page's Key, for the docs
	// there are now.
	Docs map[string]string
	// Unchanged is how many things a Targeted importer found already there.
	Unchanged int
	// Count is how many a Targeted importer made, when Created names only
	// the first of them; Into is where they went, and Show what to show
	// first (Job's Made, Into and Show).
	Count int
	Into  []Into
	Show  *Created
	// TryAgain says some pages were left out because the graph or the
	// document service failed, not because of what they hold: making them
	// again later may work (UnmakePages takes away what was made first).
	TryAgain bool
}

// MakePages makes pages into docs the way a file import does (docMaker): each
// under its parent page, or under parentUUID ("" is the top), sharing as its
// parent does, its images uploaded, its content seeded through the Live Doc
// Writer, and a doc that couldn't be filled taken away again. It is for what
// brings pages without a file job: a provider walking another tool's API
// (business/Import), the public API (POST /v1/docs).
//
// who must be able to add pages under parentUUID: ErrNotFound or ErrForbidden
// when they can't. An agent acting for who is credited beside them.
func MakePages(ctx context.Context, who accessBusiness.Subject, parentUUID string, pages []Page) (Made, error) {
	if err := checkParent(ctx, who, parentUUID); err != nil {
		return Made{}, err
	}
	person, user, err := subjectFor(ctx, who.UserUUID)
	if err != nil {
		return Made{}, ErrForbidden
	}
	person.AgentUUID = who.AgentUUID
	userID, _ := uuid.Parse(who.UserUUID)
	var kept []Page
	for _, p := range pages {
		if p.Body != nil {
			kept = append(kept, p)
		}
	}
	maker := &docMaker{who: person, user: user, userID: userID, parent: parentUUID}
	return maker.make(ctx, kept), nil
}

// UnmakePages removes the docs MakePages made, the pages under a page first,
// for what made them and starts again: it says how many it couldn't remove.
func UnmakePages(ctx context.Context, who accessBusiness.Subject, made Made) int {
	_, user, err := subjectFor(ctx, who.UserUUID)
	if err != nil {
		return len(made.Created)
	}
	left := 0
	for i := len(made.Created) - 1; i >= 0; i-- {
		if err := discardDoc(ctx, user, who, made.Created[i].UUID); err != nil {
			helpers.LogWarnWithContext(ctx, "content io: doc %s made for pages that start again stays: %v", made.Created[i].UUID, err)
			left++
		}
	}
	return left
}

// FileAssets are files read beside the pages, in memory, by the address a
// page names them by: what a provider fetched for a page (business/Import).
type FileAssets map[string][]byte

// Open opens the file at path.
func (f FileAssets) Open(path string) (io.ReadCloser, int64, error) {
	data, ok := f[path]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}
