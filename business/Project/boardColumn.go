package business

import (
	"context"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskStatusModel "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	"github.com/dgraph-io/dgo/v230"
	"github.com/google/uuid"
)

// BoardColumnPage is the next cards of one open column of a project's board.
type BoardColumnPage struct {
	Cards []*dgraphStruct.DgraphTask `json:"cards"`
	// Total is every card in the column (as the board's column count says);
	// More, whether cards follow this page.
	Total int  `json:"total"`
	More  bool `json:"more"`
}

// BoardColumnMax bounds one column page.
const BoardColumnMax = 200

// GetProjectBoardColumnPage reads the cards of one open column of a project's
// board after the card with uuid after ("" for the first), at most limit, in
// the order the board shows them (DG4b): the board's first answer sends each
// open column's first page and every column's count, and a column scrolled to
// its end asks for the next page here rather than the whole board again.
// The places of the board are read as the board reads them (one snapshot,
// the same filters), so the order is the board's. taskrank.ErrNoSuchCard,
// ErrClosedColumn and ErrUnknownColumn say why a page can't be had.
func GetProjectBoardColumnPage(ctx context.Context, projectUUID, userDgraphUID, filterQuery, status, after string, limit int) (page BoardColumnPage, err error) {
	if limit <= 0 || limit > BoardColumnMax {
		limit = BoardColumnMax
	}
	isKnown := map[string]bool{}
	if projectID, perr := uuid.Parse(projectUUID); perr == nil {
		statuses, serr := taskStatusModel.List(ctx, projectID)
		if serr != nil {
			return page, serr
		}
		for _, s := range statuses {
			isKnown[s.ID.String()] = true
		}
	}
	partOf := func(t *dgraphStruct.DgraphTask) string {
		if t.CustomStatus != nil && isKnown[*t.CustomStatus] {
			return *t.CustomStatus
		}
		return ""
	}
	err = dgraphInit.InSnapshot(ctx, func(txn *dgo.Txn) error {
		_, places, err := domain.GetDgraphProjectBoardPlaces(ctx, txn, projectUUID, userDgraphUID, filterQuery, dgraphStruct.BoardClosedLimit)
		if err != nil {
			return err
		}
		picks, total, more, err := taskrank.ColumnPage(places, status, partOf, after, limit)
		if err != nil {
			return err
		}
		uids := make([]string, len(picks))
		for i, p := range picks {
			uids[i] = p.Uid
		}
		cards, err := domain.GetDgraphProjectBoardCards(ctx, txn, uids)
		if err != nil {
			return err
		}
		filled := taskrank.Fill(map[string][]*dgraphStruct.DgraphTask{status: picks}, cards)[status]
		page = BoardColumnPage{Cards: filled, Total: total, More: more}
		return nil
	})
	if err != nil {
		return BoardColumnPage{}, err
	}
	if page.Cards == nil {
		page.Cards = []*dgraphStruct.DgraphTask{}
	}
	_ = taskBusiness.MergeGitHubMetaIntoTasks(ctx, page.Cards)
	return page, nil
}
