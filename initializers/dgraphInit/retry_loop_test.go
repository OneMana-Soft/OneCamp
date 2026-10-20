package dgraphInit

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeDgraph answers a write (Query with mutations) and a commit
// (CommitOrAbort), aborting the first abortQueries and abortCommits of them,
// or failing every write with failWith.
type fakeDgraph struct {
	api.UnimplementedDgraphServer
	abortQueries, abortCommits int32
	failWith                   error
	afterQuery                 func() // called after each write is answered, when set
	queries, commits           atomic.Int32
}

func (f *fakeDgraph) Query(ctx context.Context, req *api.Request) (*api.Response, error) {
	n := f.queries.Add(1)
	if f.afterQuery != nil {
		defer f.afterQuery()
	}
	if f.failWith != nil {
		return nil, f.failWith
	}
	if n <= f.abortQueries {
		return nil, status.Error(codes.Aborted, "Transaction has been aborted. Please retry")
	}
	return &api.Response{Txn: &api.TxnContext{StartTs: 1}}, nil
}

func (f *fakeDgraph) CommitOrAbort(ctx context.Context, tc *api.TxnContext) (*api.TxnContext, error) {
	if n := f.commits.Add(1); n <= f.abortCommits {
		return nil, status.Error(codes.Aborted, "Transaction has been aborted. Please retry")
	}
	return &api.TxnContext{StartTs: 1, CommitTs: 2}, nil
}

// useFake points DgraphClient at f until t ends.
func useFake(t *testing.T, f *fakeDgraph) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	api.RegisterDgraphServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///fake",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	live := DgraphClient
	DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))
	t.Cleanup(func() {
		DgraphClient = live
		_ = conn.Close()
		srv.Stop()
	})
}

func aWrite() *api.Request {
	return &api.Request{Mutations: []*api.Mutation{{SetJson: []byte(`{"uid":"_:n","name":"x"}`)}}}
}

// A write Dgraph aborts is tried again in a fresh transaction until it lands,
// up to abortRetries tries in all.
func TestDoCommitNowTriesAnAbortedWriteAgain(t *testing.T) {
	f := &fakeDgraph{abortQueries: abortRetries - 1}
	useFake(t, f)
	if _, err := DoCommitNow(context.Background(), aWrite()); err != nil {
		t.Fatalf("a write aborted %d times then accepted: %v", abortRetries-1, err)
	}
	if got := f.queries.Load(); got != abortRetries {
		t.Errorf("tried %d times, want %d", got, abortRetries)
	}
}

// A write aborted on every try gives up after abortRetries, saying so.
func TestDoCommitNowGivesUpAfterItsTries(t *testing.T) {
	f := &fakeDgraph{abortQueries: 1 << 20}
	useFake(t, f)
	_, err := DoCommitNow(context.Background(), aWrite())
	if !errors.Is(err, dgo.ErrAborted) {
		t.Fatalf("err = %v, want dgo.ErrAborted", err)
	}
	if got := f.queries.Load(); got != abortRetries {
		t.Errorf("tried %d times, want %d", got, abortRetries)
	}
}

// Any other failure is answered at once, without trying again.
func TestDoCommitNowAnswersOtherFailuresAtOnce(t *testing.T) {
	f := &fakeDgraph{failWith: status.Error(codes.InvalidArgument, "bad mutation")}
	useFake(t, f)
	if _, err := DoCommitNow(context.Background(), aWrite()); err == nil || errors.Is(err, dgo.ErrAborted) {
		t.Fatalf("err = %v, want the server's own error", err)
	}
	if got := f.queries.Load(); got != 1 {
		t.Errorf("tried %d times, want 1", got)
	}
}

// A caller that gives up while a write waits for its next try is answered
// with its context's error, and nothing more is sent.
func TestDoCommitNowStopsWhenTheCallerGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The caller leaves as soon as its first try is aborted.
	f := &fakeDgraph{abortQueries: 1 << 20, afterQuery: cancel}
	useFake(t, f)
	_, err := DoCommitNow(ctx, aWrite())
	// The cancel can land in the wait (ctx.Err) or on the try's own reply
	// (gRPC's Canceled); either way the caller is answered with it.
	if !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
		t.Fatalf("err = %v, want the caller's cancel", err)
	}
	if got := f.queries.Load(); got != 1 {
		t.Errorf("tried %d times, want 1: nothing after the caller left", got)
	}
}

// InTxn runs its function again, in a fresh transaction, when the commit is
// aborted, so what it read is read again; an error of its own is returned
// as it is, after one run.
func TestInTxnRunsItsWorkAgainOnAnAbort(t *testing.T) {
	f := &fakeDgraph{abortCommits: 2}
	useFake(t, f)
	runs := 0
	err := InTxn(context.Background(), func(txn *dgo.Txn) error {
		runs++
		_, err := txn.Do(context.Background(), aWrite())
		return err
	})
	if err != nil {
		t.Fatalf("InTxn: %v", err)
	}
	if runs != 3 || f.commits.Load() != 3 {
		t.Errorf("ran %d times and committed %d, want 3 and 3", runs, f.commits.Load())
	}

	own := errors.New("not allowed")
	runs = 0
	if err := InTxn(context.Background(), func(*dgo.Txn) error { runs++; return own }); !errors.Is(err, own) || runs != 1 {
		t.Errorf("err = %v after %d runs, want its own error after 1", err, runs)
	}
}
