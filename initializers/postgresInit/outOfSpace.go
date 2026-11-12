package postgresInit

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers"

	"github.com/jackc/pgx/v5"
)

// spaceTracer names, in the log, every statement Postgres refuses for space
// (SQLSTATE 53100): a parallel query's workers out of shared memory (/dev/shm),
// or a temporary file or a table out of disk. Every session of the pool
// carries it (sessionConfig), so a statement is named whichever way it went:
// a raw transaction, gorm or pgexec (V-583, main's ruling 2: one write of
// 20,442 was refused on the 64M load test, and its line couldn't say which,
// V-610).
//
// A statement is named by its caller: the first function outside the database
// layers that ran it, e.g. "models/postgres/DataTable.patchRowsTx". Never by
// its text or its values, which may hold what people wrote. The name is worked
// out only for a refused statement; any other costs one comparison.
type spaceTracer struct{}

func (spaceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (spaceTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil || !helpers.IsDiskFull(data.Err) {
		return
	}
	refusedForSpace.note(ctx, callerKind(), data.Err)
}

var (
	// ownPackage is this package's import path; the module's is its prefix.
	ownPackage = reflect.TypeOf(DB{}).PkgPath()
	module     = strings.TrimSuffix(ownPackage, "initializers/postgresInit")
	// databaseLayers are the packages a statement passes through between its
	// caller and Postgres, by the prefix of their functions' names.
	databaseLayers = []string{"runtime.", "database/sql.", "github.com/jackc/pgx/", "gorm.io/", module + "models/pgexec.", ownPackage + "."}
)

// callerKind is the kind of the statement being traced: its caller's name.
func callerKind() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	var names []string
	for {
		f, more := frames.Next()
		names = append(names, f.Function)
		if !more {
			break
		}
	}
	return firstCaller(names)
}

// firstCaller is the first of a stack's function names (innermost first)
// outside the database layers, without the module's path. Pure.
func firstCaller(names []string) string {
	for _, name := range names {
		inLayers := name == ""
		for _, layer := range databaseLayers {
			inLayers = inLayers || strings.HasPrefix(name, layer)
		}
		if !inLayers {
			return strings.TrimPrefix(name, module)
		}
	}
	return "an unnamed caller"
}

// refusedForSpace counts the statements refused for space by kind, and says
// each kind once a minute with how many came, so a Postgres short of space
// shows in the log without flooding it.
var refusedForSpace = &refusals{kinds: map[string]*refusalsOfKind{}}

type refusals struct {
	mu    sync.Mutex
	kinds map[string]*refusalsOfKind
	total atomic.Int64
}

type refusalsOfKind struct {
	said   time.Time
	unsaid int
}

// kindsKept is how many kinds refusals keeps. Kinds are callers, so a server
// has a few hundred at most; past the cap, those not said in the last minute
// are let go, and a new kind that still finds it full is said every time,
// never kept (V-589).
const kindsKept = 256

// count counts one refusal of kind at now: whether to say it, with how many
// of the kind came since its last line, and how many of any kind since the
// server started.
func (r *refusals) count(kind string, now time.Time) (say bool, n int, total int64) {
	total = r.total.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.kinds[kind]
	if k == nil {
		if len(r.kinds) >= kindsKept {
			for name, old := range r.kinds {
				if now.Sub(old.said) >= time.Minute {
					delete(r.kinds, name)
				}
			}
		}
		if len(r.kinds) >= kindsKept {
			return true, 1, total
		}
		k = &refusalsOfKind{}
		r.kinds[kind] = k
	}
	k.unsaid++
	if now.Sub(k.said) < time.Minute {
		return false, 0, total
	}
	n, k.said, k.unsaid = k.unsaid, now, 0
	return true, n, total
}

func (r *refusals) note(ctx context.Context, kind string, err error) {
	say, n, total := r.count(kind, time.Now())
	if !say {
		return
	}
	helpers.LogWarnWithContext(ctx, "postgres: out of space (53100) in %s: %d such refusals of it since its last line, "+
		"%d of any statement since the server started. %s Its error: %v", kind, n, total, spaceAdvice(err), err)
}

// spaceAdvice is what Postgres needs more of for err: shared memory when a
// parallel query's workers ran out of it, its disk otherwise. Pure.
func spaceAdvice(err error) string {
	if strings.Contains(err.Error(), "shared memory segment") {
		return "Give Postgres a /dev/shm of at least 256M: its parallel workers share memory there."
	}
	return "Free space on Postgres' disk: a temporary file or a table could not grow."
}
