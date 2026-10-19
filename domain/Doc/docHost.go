package domain

// Hosted docs (docs D-18): a doc that belongs to something else, set once when
// it is made. A table row's page is hosted by its row ("table_row", the row's
// id). Its level is its host's, which business/Doc asks only on a path that
// opens one (docHost.go there); the rule here gives it none (DecideDoc), so
// every other path, from the lists to sharing, finds it open to nobody. It is
// made with no maker, owner, shares, space or parent, private, so no list
// built from those ever holds it, and the lists that walk every doc leave it
// out by its host (notInternalFilter, inactiveDocFilter, the search writers).

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// ErrNoHost is a host a hosted doc can't be made for: no kind, or an id that
// isn't a uuid.
var ErrNoHost = errors.New("doc: no such host")

// hostKindPattern is what a host's kind may be: a resource kind's name.
var hostKindPattern = regexp.MustCompile(`^[a-z_]{1,32}$`)

// IsHosted is whether a doc belongs to a host. doc must be read with
// doc_host_kind, as every decision read is (accessScalars).
func IsHosted(doc *dgraphStruct.DgraphDoc) bool {
	return doc != nil && doc.HostKind != ""
}

// HostOfDoc is the host a doc belongs to, its kind and id, both "" for a doc
// that has none; ErrNotFound for no doc.
func HostOfDoc(ctx context.Context, docUUID string) (kind, host string, err error) {
	if _, perr := uuid.Parse(docUUID); perr != nil {
		return "", "", ErrNotFound
	}
	doc, err := dgraphModels.GetDgraphDocInfoByUUID(ctx, `query HostOf($id: string) {
		docInfo(func: eq(doc_uuid, $id)) { doc_uuid doc_host_kind doc_host_uuid }
	}`, map[string]string{"$id": docUUID})
	if err != nil {
		return "", "", err
	}
	if doc == nil || doc.Uuid == "" {
		return "", "", ErrNotFound
	}
	return doc.HostKind, doc.HostUUID, nil
}

// hostedDocRequest is EnsureHostedDoc's upsert: the doc made, with docUUID and
// title, only when the host has none, and the host's doc as found before. The
// host's id is @upsert in the schema, so two makes of one host's doc at once
// conflict: Dgraph aborts the second, and EnsureHostedDoc runs it again in a
// fresh transaction (commitHostedDoc, dgraphInit.DoCommitNow), whose query
// finds the first. Pure.
func hostedDocRequest(kind, host, docUUID, title string, at time.Time) (*api.Request, error) {
	if !hostKindPattern.MatchString(kind) {
		return nil, ErrNoHost
	}
	if _, err := uuid.Parse(host); err != nil {
		return nil, ErrNoHost
	}
	if _, err := uuid.Parse(docUUID); err != nil {
		return nil, ErrNoDocToUpdate
	}
	private, comment := true, false
	live := time.Time{}
	made := at.UTC()
	set, err := json.Marshal(&dgraphStruct.DgraphDoc{
		Uid:           "_:page",
		DType:         []string{"Doc"},
		Uuid:          docUUID,
		Title:         title,
		IsPrivate:     &private,
		PublicComment: &comment,
		HostKind:      kind,
		HostUUID:      host,
		CreatedAt:     &made,
		UpdatedAt:     &made,
		DeletedAt:     &live,
	})
	if err != nil {
		return nil, err
	}
	return &api.Request{
		Query: `query q($kind: string, $host: string) {
			hosted as var(func: eq(doc_host_uuid, $host)) @filter(eq(doc_host_kind, $kind))
			found(func: uid(hosted), first: 1) { doc_uuid }
		}`,
		Vars:      map[string]string{"$kind": kind, "$host": host},
		Mutations: []*api.Mutation{{SetJson: set, Cond: "@if(eq(len(hosted), 0))"}},
	}, nil
}

// commitHostedDoc commits EnsureHostedDoc's upsert, and runs it again in a
// fresh transaction when Dgraph aborts it (dgo.ErrAborted, up to three tries
// in all): a seam, so a test can be the graph.
var commitHostedDoc = dgraphInit.DoCommitNow

// EnsureHostedDoc is the doc a host has, made now (as docUUID, titled title)
// when it has none: one doc a host, however many ask at once. made says
// whether this call made it; a make that conflicted with another's, run
// again, finds that one's doc and says it didn't.
func EnsureHostedDoc(ctx context.Context, kind, host, docUUID, title string, at time.Time) (string, bool, error) {
	req, err := hostedDocRequest(kind, host, docUUID, title, at)
	if err != nil {
		return "", false, err
	}
	resp, err := commitHostedDoc(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/EnsureHostedDoc %s %s err: %+v", kind, host, err)
		return "", false, err
	}
	var found struct {
		Found []struct {
			UUID string `json:"doc_uuid"`
		} `json:"found"`
	}
	if err := json.Unmarshal(resp.GetJson(), &found); err != nil {
		return "", false, err
	}
	if len(found.Found) > 0 && found.Found[0].UUID != "" {
		return found.Found[0].UUID, false, nil
	}
	return docUUID, true, nil
}
