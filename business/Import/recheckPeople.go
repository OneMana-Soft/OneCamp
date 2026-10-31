package business

// Old Basecamp imports' clients, re-checked against Basecamp (P-IMP-6D: design
// release-1010/design/permissions/REMEDIATION-P-IMP-3D-6D-2026-10-25.md).
// Before Basecamp's fix (BasecampClientsFixed), an import made each person of
// a project a member of it here, Basecamp's clients too, who see only what's
// shared with clients there and everything in a project here. The re-check
// changes no task. With the import's connection, or an admin's, it proves
// the source is the import's own account (most of a sample of its projects
// answer there), then lists on 510's list, for an admin to decide
// (business/ImportReview), each person the import added who is still in one
// of its projects and whom Basecamp keeps out of projects (reason
// basecamp_client) or no longer lists (as 510 lists everyone an import
// added), and drops the undecided guesses of those Basecamp lets in. An
// import that can't be re-checked lists everyone it added (toAdmins).
// Nobody is taken off without an admin's say, and nothing is opened.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// membersAmongEach is who of some people are in each of some projects. A seam
// for tests.
var membersAmongEach = projectBusiness.MembersAmongEach

// recheckPeople re-checks an import of people with r's connection, and
// settles it with each project's counts.
func (r *recheck) recheckPeople(ctx context.Context, units []*recheckUnit) error {
	rp, ok := importProvider.Get(r.job.Provider).(importProvider.PeopleRechecker)
	if !ok {
		return fmt.Errorf("%s has no re-check of its people", r.job.Provider)
	}
	now, err := samePeople(ctx, rp, r.job, r.cred)
	if err != nil {
		return r.failed(ctx, units, err)
	}
	listed, err := listPeople(ctx, r.job.Id, now.KeptOut)
	if err != nil {
		return r.failed(ctx, units, err)
	}
	counts := make(map[uuid.UUID]any, len(units))
	for _, u := range units {
		u.peopleCounts.Clients, u.peopleCounts.PeopleUnlisted = listed[u.project].clients, listed[u.project].unlisted
		counts[u.people.ID] = u.peopleCounts
	}
	if err := reviewModel.SetAccessSummaries(ctx, counts); err != nil {
		return err
	}
	if _, err := reviewModel.MoveAccessReviews(ctx, r.job.Id, []string{reviewModel.StateRechecking}, reviewModel.StateRechecked,
		map[string]any{"failures": 0, "retries": 0}, nil); err != nil {
		return err
	}
	r.recordDone(ctx, units)
	return nil
}

// samePeople reads the import's people as its source has them now, with
// cred's connection, once it reaches the import's own account: most of a
// sample of the projects the import made answer there. (Every import
// re-checked has a project: its reviews are per project.)
func samePeople(ctx context.Context, rp importProvider.PeopleRechecker, job *importModels.Job, cred uuid.UUID) (importProvider.RecheckedPeople, error) {
	projects, err := importModels.IdMapOf(ctx, job.Id, importModels.EntityProject)
	if err != nil {
		return importProvider.RecheckedPeople{}, err
	}
	sample := slices.Sorted(maps.Keys(projects))
	sample = sample[:min(len(sample), recheckSample)]
	now, err := waitOut(ctx, func() (importProvider.RecheckedPeople, error) { return rp.RecheckPeople(ctx, cred, job, sample) })
	if err == nil && 2*len(now.Answered) <= len(sample) {
		err = errOtherSource
	}
	return now, err
}

// peopleListed is what an import's people's re-check listed in one project:
// clients, and people the source no longer lists.
type peopleListed struct{ clients, unlisted int }

// listPeople lists on 510's list each person the import added who is still in
// one of its projects, by how its source has its people now (keptOut, by
// source id): a client, with reason basecamp_client; and one it no longer
// lists, as a guess without one. It drops the undecided guesses of the people
// it lets in, and answers what it listed by project.
func listPeople(ctx context.Context, importID uuid.UUID, keptOut map[string]bool) (map[uuid.UUID]peopleListed, error) {
	users, err := importModels.IdMapOf(ctx, importID, importModels.EntityUser)
	if err != nil {
		return nil, err
	}
	clients, joining, unlisted := sortPeople(users, keptOut)
	cands, err := reviewModel.CandidatesIn(ctx, importID, append(slices.Collect(maps.Keys(clients)), slices.Collect(maps.Keys(unlisted))...))
	if err != nil {
		return nil, err
	}
	for i := range cands {
		if clients[cands[i].UserID] {
			cands[i].Reason = reviewModel.ReasonBasecampClient
		}
	}
	still, err := addStill(ctx, cands)
	if err != nil {
		return nil, err
	}
	if _, err := reviewModel.DropGuesses(ctx, importID, slices.Collect(maps.Keys(joining))); err != nil {
		return nil, err
	}
	listed := map[uuid.UUID]peopleListed{}
	for _, c := range still {
		n := listed[c.ProjectUUID]
		if c.Reason != "" {
			n.clients++
		} else {
			n.unlisted++
		}
		listed[c.ProjectUUID] = n
	}
	return listed, nil
}

// listEveryone lists on 510's list, as a guess, everyone some imports of
// people added who is still in one of their projects, as 510 lists everyone
// an import added: what an import that can't be re-checked lists for its
// admins. Nothing here tells a client among them: every person an import
// made is external, and the source's client flag was never recorded.
func listEveryone(ctx context.Context, importIDs []uuid.UUID) error {
	cands, err := reviewModel.AddedBy(ctx, importIDs)
	if err != nil {
		return err
	}
	_, err = addStill(ctx, cands)
	return err
}

// addStill puts on 510's list those of cands still in their project, and
// answers them.
func addStill(ctx context.Context, cands []reviewModel.Candidate) ([]reviewModel.Candidate, error) {
	still, err := StillMembers(ctx, cands)
	if err != nil {
		return nil, err
	}
	if _, err := reviewModel.Add(ctx, still); err != nil {
		return nil, err
	}
	return still, nil
}

// sortPeople sorts an import's people (users: each source person to whom
// they came across as) by how its source has them now (keptOut): clients,
// one of whose source people it keeps out of projects; joining, whom it lets
// in and keeps none of out; and unlisted, whom it no longer lists at all.
// Pure.
func sortPeople(users map[string]uuid.UUID, keptOut map[string]bool) (clients, joining, unlisted map[uuid.UUID]bool) {
	clients, joining, unlisted = map[uuid.UUID]bool{}, map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for source, user := range users {
		switch out, listed := keptOut[source]; {
		case out:
			clients[user] = true
		case listed:
			joining[user] = true
		default:
			unlisted[user] = true
		}
	}
	for user := range clients {
		delete(joining, user)
		delete(unlisted, user)
	}
	for user := range joining {
		delete(unlisted, user)
	}
	return clients, joining, unlisted
}

// StillMembers keeps the candidates who are still in their project, asking
// the graph about all their projects at once (a read a page of projects).
// 510's scan and a re-check of an import's people share it.
func StillMembers(ctx context.Context, cands []reviewModel.Candidate) ([]reviewModel.Candidate, error) {
	asked := map[string][]string{}
	for _, c := range cands {
		p := c.ProjectUUID.String()
		asked[p] = append(asked[p], c.UserID.String())
	}
	in, err := membersAmongEach(ctx, asked)
	if err != nil {
		return nil, err
	}
	var out []reviewModel.Candidate
	for _, c := range cands {
		if in[c.ProjectUUID.String()][c.UserID.String()] {
			out = append(out, c)
		}
	}
	return out, nil
}
