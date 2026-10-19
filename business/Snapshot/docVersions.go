package business

// Doc versions: the first kind of resource the Snapshot Service keeps.
//
// WHEN A VERSION IS TAKEN.
//   - On a save that changed the body, at most every 10 minutes while the doc is
//     edited ("interval"), and the body before a save that deleted most of it
//     ("mass_delete"): the doc save hook below.
//   - 2 minutes after the last edit ("idle"): the idle sweep.
//   - Before a server-side write replaces or adds to the body (an API write, an
//     agent, an import: "before_write"), or before a restore ("manual"), from the
//     live copy as the write found it: the Live Doc Writer hands the version over
//     (services/collab KeepVersion). An agent's is an agent_run version, with the
//     run's id, which its Revert restores.
//   - When someone keeps one on purpose ("saved", possibly named).
//
// Each version is credited from the ledger (ledger.go) and kept by the policy
// (retention.go). Seeing, naming and restoring a doc's versions takes edit access,
// asked of the Access Resolver; a doc in the trash keeps its history there.

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const (
	// docKind is the resource kind docs register under (accessBusiness.KindDoc).
	docKind = accessBusiness.KindDoc
	// intervalEvery is how often an edited doc gets a version.
	intervalEvery = 10 * time.Minute
	// idleAfter is how long after the last edit a doc gets one.
	idleAfter = 2 * time.Minute
	// listLimit is the most versions a list answers.
	listLimit = 200
)

func init() {
	Register(docKind, docVersions{})
	// A row's page keeps its versions too (a hosted doc, D-18): they keep the
	// doc and put it nowhere it is found.
	docBusiness.SaveHooks.RegisterAlsoHosted("doc.versions", onDocSave)
	collab.KeepVersion = keepVersionBeforeWrite
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "doc.versions.idle", Every: 30 * time.Second, Run: takeIdleVersions})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "doc.versions.prune", Every: 6 * time.Hour, Run: pruneAllDocVersions})
}

// docStore is where doc versions live: the index in Postgres, the bodies in
// object storage. A test swaps it.
type docStore interface {
	// put keeps a body and its row; the row's ObjectKey and sizes are filled.
	put(ctx context.Context, row *snapshotModels.DocSnapshot, body string) (uuid.UUID, error)
	latest(ctx context.Context, docUUID string) (*snapshotModels.DocSnapshot, error)
	list(ctx context.Context, docUUID string, limit int) ([]*snapshotModels.DocSnapshot, error)
	get(ctx context.Context, docUUID string, id uuid.UUID) (*snapshotModels.DocSnapshot, error)
	body(ctx context.Context, docUUID string, id uuid.UUID) (string, error)
	setName(ctx context.Context, docUUID string, id uuid.UUID, name *string, pinned bool) (bool, error)
	keptBytes(ctx context.Context, docUUID string) (int64, error)
	retention(ctx context.Context, docUUID string) ([]snapshotModels.RetentionRow, error)
	remove(ctx context.Context, ids []uuid.UUID) error
	docsBefore(ctx context.Context, cutoff time.Time, after string, limit int) ([]string, error)
}

var store docStore = pgStore{}

// Seams for what a doc version reads from elsewhere.
var (
	// storedBody is the doc's stored body (for a version the server takes when
	// the live copy cannot be read).
	storedBody = func(ctx context.Context, docUUID string) (string, error) {
		doc, err := docBusiness.GetSystemDocByUUID(ctx, docUUID)
		if err != nil {
			return "", err
		}
		if doc == nil {
			return "", ErrNotFound
		}
		return doc.Body, nil
	}
	now = time.Now
)

// docVersions is the doc Resource.
type docVersions struct{}

func (docVersions) Allow(ctx context.Context, who accessBusiness.Subject, docUUID string) error {
	if _, err := uuid.Parse(docUUID); err != nil {
		return ErrNotFound
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	if errors.Is(err, docBusiness.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.Deleted {
		// In the trash: said so to someone it was there for (the resolver's
		// level before the deletion, Was), and not found to anyone else, as
		// every read of a deleted doc answers.
		if d.Owner || d.Was >= accessBusiness.LevelView || d.Level > accessBusiness.LevelNone {
			return ErrInTrash
		}
		return ErrNotFound
	}
	if d.Level < accessBusiness.LevelEdit {
		if d.Level == accessBusiness.LevelNone {
			return ErrNotFound
		}
		return ErrForbidden
	}
	return nil
}

func (docVersions) Take(ctx context.Context, t Take) (Version, error) {
	body, lineage := t.Body, t.Lineage
	if strings.TrimSpace(body) == "" {
		cur, err := collab.Read(ctx, t.ResourceUUID)
		switch {
		case err == nil:
			body, lineage = cur.HTML, cur.Lineage
		case errors.Is(err, collab.ErrNoSuchDoc):
			return Version{}, ErrNotFound
		default:
			// The live copy cannot be read: the stored body is the doc as of
			// its last save.
			if body, err = storedBody(ctx, t.ResourceUUID); err != nil {
				return Version{}, err
			}
		}
	}
	if strings.TrimSpace(body) == "" {
		return Version{}, fmt.Errorf("%w: the doc is empty", ErrNotFound)
	}
	reason := t.Reason
	if reason == "" {
		reason = snapshotModels.ReasonSaved
	}
	kind := t.Kind
	if kind == "" {
		kind = KindDoc
	}
	return keepDocVersion(ctx, t.ResourceUUID, body, keepAs{
		reason: reason, kind: kind, name: t.Name, pinned: t.Pinned, runID: t.RunID, lineage: lineage, by: t.By,
	})
}

func (docVersions) List(ctx context.Context, docUUID string) ([]Version, error) {
	rows, err := store.list(ctx, docUUID, listLimit)
	if err != nil {
		return nil, err
	}
	out := make([]Version, 0, len(rows))
	for _, r := range rows {
		out = append(out, versionOf(r))
	}
	return out, nil
}

func (docVersions) Preview(ctx context.Context, docUUID, versionID string) (string, error) {
	id, err := uuid.Parse(versionID)
	if err != nil {
		return "", ErrNotFound
	}
	return store.body(ctx, docUUID, id)
}

func (docVersions) Name(ctx context.Context, docUUID, versionID, name string, pinned bool) error {
	id, err := uuid.Parse(versionID)
	if err != nil {
		return ErrNotFound
	}
	row, err := store.get(ctx, docUUID, id)
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	name = strings.TrimSpace(name)
	var named *string
	if name != "" {
		named = &name
	}
	keep := pinned || named != nil
	wasKept := row.Pinned || (row.Name != nil && *row.Name != "")
	if keep && !wasKept {
		kept, err := store.keptBytes(ctx, docUUID)
		if err != nil {
			return err
		}
		if limit := docPolicy().MaxBytes; limit > 0 && kept+int64(row.StoredBytes) > limit {
			return ErrKeptFull
		}
	}
	found, err := store.setName(ctx, docUUID, id, named, pinned)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// Restore makes a version the doc's body, live, keeping the body it replaced as
// a "manual" version first (so the restore can be undone).
func (docVersions) Restore(ctx context.Context, who accessBusiness.Subject, docUUID, versionID string) error {
	id, err := uuid.Parse(versionID)
	if err != nil {
		return ErrNotFound
	}
	body, err := store.body(ctx, docUUID, id)
	if err != nil {
		return err
	}
	if _, err := collab.Live.ReplaceAll(ctx, docUUID, body, collab.Options{
		By:            collab.By{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		SnapshotFirst: true, SnapshotReason: snapshotModels.ReasonManual,
	}); err != nil {
		return err
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocVersionRestore, ResourceKind: docKind, ResourceUUID: docUUID,
		Actor: resourceEventBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		At:    now(), Data: map[string]string{"version": versionID},
	})
	return nil
}

// keepAs is how a version is kept.
type keepAs struct {
	reason  string
	kind    Kind
	name    string
	pinned  bool
	runID   string
	lineage string
	by      Contributor
}

// keepDocVersion keeps body as a version: credited to everyone in the ledger
// since the last version (and to `by`), then the doc's versions are pruned.
func keepDocVersion(ctx context.Context, docUUID, body string, k keepAs) (Version, error) {
	who := ledger.Drain(ctx, docKind, docUUID)
	if k.by.UserUUID != "" || k.by.AgentUUID != "" {
		who = sortContributors(append(who, k.by))
	}
	row := &snapshotModels.DocSnapshot{
		DocUUID: docUUID, Reason: k.reason, Kind: string(k.kind), RunID: k.runID, Lineage: k.lineage,
		Pinned: k.pinned, BodyBytes: len(body), CreatedAt: now(),
		Contributors: modelContributors(who), ContributorUUIDs: peopleOf(who),
	}
	if name := strings.TrimSpace(k.name); name != "" {
		row.Name = &name
	}
	if _, err := store.put(ctx, row, body); err != nil {
		// What was drained goes back, for the next version.
		ledger.Add(ctx, docKind, docUUID, who)
		return Version{}, err
	}
	pruneDocVersions(ctx, docUUID)
	return versionOf(row), nil
}

// onDocSave is the doc save hook: the ledger, and the interval and mass-delete
// versions. It runs in the background after a save that changed the body.
func onDocSave(ctx context.Context, c docBusiness.Change) {
	who := make([]Contributor, 0, len(c.Contributors)+len(c.Agents))
	for _, u := range c.Contributors {
		who = append(who, Contributor{UserUUID: u})
	}
	for _, a := range c.Agents {
		who = append(who, Contributor{AgentUUID: a.AgentUUID, AskerUUID: a.AskerUUID})
	}
	ledger.Add(ctx, docKind, c.DocUUID, who)
	marks.Mark(ctx, docKind, c.DocUUID, c.SavedAt)

	if c.Previous != "" {
		// Most of the doc just went: keep what it was.
		if _, err := keepDocVersion(ctx, c.DocUUID, c.Previous, keepAs{reason: snapshotModels.ReasonMassDelete, kind: KindDoc}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Snapshot mass-delete version of %s failed: %v", c.DocUUID, err)
		}
		return
	}
	latest, err := store.latest(ctx, c.DocUUID)
	if err != nil {
		return
	}
	if latest == nil || now().Sub(latest.CreatedAt) >= intervalEvery {
		if _, err := keepDocVersion(ctx, c.DocUUID, c.Body, keepAs{reason: snapshotModels.ReasonInterval, kind: KindDoc, lineage: c.Lineage}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Snapshot interval version of %s failed: %v", c.DocUUID, err)
		}
	}
}

// takeIdleVersions keeps a version of each doc left alone for idleAfter since
// its last change, unless one was kept since that change.
func takeIdleVersions(ctx context.Context) error {
	for _, m := range marks.Due(ctx, docKind, now().Add(-idleAfter), 100) {
		if !marks.Claim(ctx, docKind, m) {
			continue
		}
		latest, err := store.latest(ctx, m.UUID)
		if err != nil {
			continue
		}
		if latest != nil && !latest.CreatedAt.Before(time.UnixMilli(m.At)) {
			continue
		}
		body, err := storedBody(ctx, m.UUID)
		if err != nil || strings.TrimSpace(body) == "" {
			continue
		}
		if _, err := keepDocVersion(ctx, m.UUID, body, keepAs{reason: snapshotModels.ReasonIdle, kind: KindDoc}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Snapshot idle version of %s failed: %v", m.UUID, err)
		}
	}
	return nil
}

// keepVersionBeforeWrite keeps the body a Live Doc Writer op found, taken in the
// same step as its write (services/collab KeepVersion).
func keepVersionBeforeWrite(ctx context.Context, v collab.Version) (string, error) {
	reason := v.Reason
	if reason == "" {
		reason = snapshotModels.ReasonBeforeWrite
	}
	// The ledger's people edited the body this version holds. The write itself
	// comes after it, so it is credited to the next version (it reaches the
	// ledger with the save that stores it). An agent run's version also names
	// the agent and who asked it, so its Revert can say whose run it undoes.
	k := keepAs{reason: reason, kind: KindDoc, name: v.Name, runID: v.RunID, lineage: v.Lineage}
	if v.Agent {
		k.kind = KindAgentRun
		k.by = Contributor{AgentUUID: v.By.AgentUUID, AskerUUID: v.By.UserUUID}
	}
	ver, err := keepDocVersion(ctx, v.DocUUID, v.HTML, k)
	return ver.ID, err
}

// pruneDocVersions applies the policy to one doc.
func pruneDocVersions(ctx context.Context, docUUID string) {
	rows, err := store.retention(ctx, docUUID)
	if err != nil || len(rows) == 0 {
		return
	}
	plan := make([]planRow, 0, len(rows))
	for _, r := range rows {
		plan = append(plan, planRow{ID: r.ID, AgentRun: r.Kind == string(KindAgentRun), Kept: r.Kept, Bytes: int64(r.StoredBytes), CreatedAt: r.CreatedAt})
	}
	if drop := planPrune(plan, now(), docPolicy()); len(drop) > 0 {
		if err := store.remove(ctx, drop); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Snapshot pruning %s failed: %v", docUUID, err)
		}
	}
}

// pruneAllDocVersions applies the policy to every doc with versions old enough
// to thin: the docs nobody edits any more age out too.
func pruneAllDocVersions(ctx context.Context) error {
	cutoff := now().Add(-everyVersionFor)
	after := ""
	for {
		docs, err := store.docsBefore(ctx, cutoff, after, 200)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pruneDocVersions(ctx, d)
		}
		if len(docs) < 200 {
			return nil
		}
		after = docs[len(docs)-1]
	}
}

func versionOf(r *snapshotModels.DocSnapshot) Version {
	v := Version{
		ID: r.ID.String(), ResourceKind: docKind, ResourceUUID: r.DocUUID, Kind: Kind(r.Kind),
		Pinned: r.Pinned, Lineage: r.Lineage, SizeBytes: r.BodyBytes, Reason: r.Reason, RunID: r.RunID,
		CreatedAt: r.CreatedAt, Contributors: make([]Contributor, 0, len(r.Contributors)),
	}
	if r.Name != nil {
		v.Name = *r.Name
	}
	if v.Kind == "" {
		v.Kind = KindDoc
	}
	for _, c := range r.Contributors {
		v.Contributors = append(v.Contributors, Contributor{UserUUID: c.UserUUID, AgentUUID: c.AgentUUID, AskerUUID: c.AskerUUID})
	}
	// Versions taken before the ledger carried only uuids.
	if len(v.Contributors) == 0 {
		for _, u := range r.ContributorUUIDs {
			v.Contributors = append(v.Contributors, Contributor{UserUUID: u})
		}
	}
	return v
}

func modelContributors(in []Contributor) []snapshotModels.Contributor {
	out := make([]snapshotModels.Contributor, 0, len(in))
	for _, c := range in {
		out = append(out, snapshotModels.Contributor{UserUUID: c.UserUUID, AgentUUID: c.AgentUUID, AskerUUID: c.AskerUUID})
	}
	return out
}

// peopleOf is the people a version credits: those who edited, and those who
// asked an agent to.
func peopleOf(in []Contributor) []string {
	seen := map[string]bool{"": true}
	var out []string
	for _, c := range in {
		for _, u := range []string{c.UserUUID, c.AskerUUID} {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// pgStore is the doc store: Postgres for the index, object storage for bodies.
type pgStore struct{}

func bucket() string { return helpers.UserUploadBucket() }

func (pgStore) put(ctx context.Context, row *snapshotModels.DocSnapshot, body string) (uuid.UUID, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		return uuid.Nil, err
	}
	if err := zw.Close(); err != nil {
		return uuid.Nil, err
	}
	row.ID = uuid.New()
	row.ObjectKey = fmt.Sprintf("docSnapshot/%s/%s.gz", row.DocUUID, row.ID)
	row.StoredBytes = buf.Len()
	if _, err := minioInit.MinioClient.PutObject(ctx, bucket(), row.ObjectKey, bytes.NewReader(buf.Bytes()), int64(buf.Len()),
		minio.PutObjectOptions{ContentType: "application/gzip"}); err != nil {
		return uuid.Nil, err
	}
	id, err := snapshotModels.CreateSnapshot(ctx, row)
	if err != nil {
		_ = minioInit.MinioClient.RemoveObject(context.WithoutCancel(ctx), bucket(), row.ObjectKey, minio.RemoveObjectOptions{})
		return uuid.Nil, err
	}
	return id, nil
}

func (pgStore) latest(ctx context.Context, docUUID string) (*snapshotModels.DocSnapshot, error) {
	return snapshotModels.GetLatestSnapshot(ctx, docUUID)
}

func (pgStore) list(ctx context.Context, docUUID string, limit int) ([]*snapshotModels.DocSnapshot, error) {
	return snapshotModels.ListSnapshots(ctx, docUUID, limit)
}

func (pgStore) get(ctx context.Context, docUUID string, id uuid.UUID) (*snapshotModels.DocSnapshot, error) {
	return snapshotModels.GetSnapshotByID(ctx, id, docUUID)
}

func (pgStore) body(ctx context.Context, docUUID string, id uuid.UUID) (string, error) {
	row, err := snapshotModels.GetSnapshotByID(ctx, id, docUUID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return docBusiness.ReadDocVersionBody(ctx, docUUID, id)
}

func (pgStore) setName(ctx context.Context, docUUID string, id uuid.UUID, name *string, pinned bool) (bool, error) {
	return snapshotModels.SetSnapshotName(ctx, id, docUUID, name, pinned)
}

func (pgStore) keptBytes(ctx context.Context, docUUID string) (int64, error) {
	return snapshotModels.KeptBytes(ctx, docUUID)
}

func (pgStore) retention(ctx context.Context, docUUID string) ([]snapshotModels.RetentionRow, error) {
	return snapshotModels.ListForRetention(ctx, docUUID)
}

func (pgStore) remove(ctx context.Context, ids []uuid.UUID) error {
	keys, err := snapshotModels.DeleteUnkeptByIDs(ctx, ids)
	for _, k := range keys {
		_ = minioInit.MinioClient.RemoveObject(context.WithoutCancel(ctx), bucket(), k, minio.RemoveObjectOptions{})
	}
	return err
}

func (pgStore) docsBefore(ctx context.Context, cutoff time.Time, after string, limit int) ([]string, error) {
	return snapshotModels.DocsWithVersionsBefore(ctx, cutoff, after, limit)
}
