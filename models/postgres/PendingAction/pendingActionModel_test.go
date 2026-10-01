package models

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExpired(t *testing.T) {
	past := &PendingAction{ExpiresAt: time.Now().Add(-time.Minute)}
	if !past.Expired() {
		t.Fatal("expected an action past its expiry to report Expired()=true")
	}
	future := &PendingAction{ExpiresAt: time.Now().Add(time.Hour)}
	if future.Expired() {
		t.Fatal("expected an action before its expiry to report Expired()=false")
	}
}

// fakeScanner feeds a fixed set of column values into scanAction so we can test
// the row-mapping (jsonb params decode, nullable columns) without a database.
type fakeScanner struct {
	id          uuid.UUID
	requestedBy uuid.UUID
	surfaceType string
	surfaceID   string
	toolName    string
	params      []byte
	description string
	status      string
	idem        sql.NullString
	result      sql.NullString
	errStr      sql.NullString
	expiresAt   time.Time
	createdAt   time.Time
	resolvedAt  sql.NullTime
	resolvedBy  uuid.NullUUID
	agentID     uuid.NullUUID
	runID       uuid.NullUUID
}

func (f *fakeScanner) Scan(dest ...any) error {
	vals := []any{f.id, f.requestedBy, f.surfaceType, f.surfaceID, f.toolName,
		f.params, f.description, f.status, f.idem, f.result, f.errStr,
		f.expiresAt, f.createdAt, f.resolvedAt, f.resolvedBy, f.agentID, f.runID}
	for i := range dest {
		switch d := dest[i].(type) {
		case *uuid.UUID:
			*d = vals[i].(uuid.UUID)
		case *string:
			*d = vals[i].(string)
		case *[]byte:
			*d = vals[i].([]byte)
		case *sql.NullString:
			*d = vals[i].(sql.NullString)
		case *time.Time:
			*d = vals[i].(time.Time)
		case *sql.NullTime:
			*d = vals[i].(sql.NullTime)
		case *uuid.NullUUID:
			*d = vals[i].(uuid.NullUUID)
		default:
			t := dest[i]
			_ = t
		}
	}
	return nil
}

func TestScanActionMapsParamsAndNulls(t *testing.T) {
	fs := &fakeScanner{
		id:          uuid.New(),
		requestedBy: uuid.New(),
		surfaceType: SurfaceChannel,
		surfaceID:   "chan-1",
		toolName:    "create_task",
		params:      []byte(`{"title":"Ship it","project":"p1"}`),
		description: "Create a task",
		status:      StatusPending,
		expiresAt:   time.Now().Add(time.Hour),
		createdAt:   time.Now(),
	}
	p, err := scanAction(fs)
	if err != nil {
		t.Fatalf("scanAction err: %v", err)
	}
	if p.ToolName != "create_task" || p.Status != StatusPending {
		t.Fatalf("unexpected scalar mapping: %+v", p)
	}
	if p.Params["title"] != "Ship it" || p.Params["project"] != "p1" {
		t.Fatalf("params not decoded: %+v", p.Params)
	}
	if p.IdempotencyKey != nil || p.Result != nil || p.Error != nil || p.ResolvedBy != nil {
		t.Fatalf("expected null columns to map to nil pointers: %+v", p)
	}
}

func TestScanActionEmptyParams(t *testing.T) {
	// A NULL/empty params blob must yield an empty (non-nil) map, never panic.
	fs := &fakeScanner{
		id:          uuid.New(),
		requestedBy: uuid.New(),
		surfaceType: SurfaceDM,
		toolName:    "send_dm",
		params:      nil,
		status:      StatusPending,
		expiresAt:   time.Now().Add(time.Hour),
		createdAt:   time.Now(),
	}
	p, err := scanAction(fs)
	if err != nil {
		t.Fatalf("scanAction err: %v", err)
	}
	if p.Params == nil || len(p.Params) != 0 {
		t.Fatalf("expected empty non-nil params, got %+v", p.Params)
	}
}

// Attribution is what makes an approval usable as a quality signal, so the two
// columns have to survive the read path. They are nullable and most rows have
// neither, which is exactly the shape that gets silently dropped by a scanner.
func TestScanActionCarriesAgentAttribution(t *testing.T) {
	agentID, runID := uuid.New(), uuid.New()
	fs := &fakeScanner{
		id: uuid.New(), requestedBy: uuid.New(),
		surfaceType: SurfaceChannel, surfaceID: "chan-1",
		toolName: "create_task", params: []byte(`{}`), status: StatusPending,
		agentID: uuid.NullUUID{UUID: agentID, Valid: true},
		runID:   uuid.NullUUID{UUID: runID, Valid: true},
	}
	p, err := scanAction(fs)
	if err != nil {
		t.Fatalf("scanAction: %v", err)
	}
	if p.AgentID == nil || *p.AgentID != agentID {
		t.Errorf("AgentID = %v, want %v", p.AgentID, agentID)
	}
	if p.RunID == nil || *p.RunID != runID {
		t.Errorf("RunID = %v, want %v", p.RunID, runID)
	}

	// The common case: the assistant proposed it, so there is no agent. Nil,
	// not a zero uuid, or every non-agent proposal would attribute itself to
	// the same imaginary agent.
	fs.agentID, fs.runID = uuid.NullUUID{}, uuid.NullUUID{}
	p, err = scanAction(fs)
	if err != nil {
		t.Fatalf("scanAction: %v", err)
	}
	if p.AgentID != nil || p.RunID != nil {
		t.Errorf("unattributed action got AgentID=%v RunID=%v, want both nil", p.AgentID, p.RunID)
	}
}
