//go:build integration
// +build integration

package integration

// The binary's servers: one of each kind, started on first use and used by
// the tests one after another, each test beginning from what a new server
// holds.
//
// ONE SET OF SERVERS A RUN. Every Setup used to start a container of its own,
// and the tests spent most of their time waiting for them. In the kept raw
// logs of 2,539 test runs (13–17 Oct 2026), starting containers took 32% of
// all test time, applying the 219 migrations to each new Postgres 27% and
// stopping the containers 14%: a test that read one row took 40 s. Now each
// server starts once, and a test still begins from what a new one holds:
//
//   - Postgres: a database of its own, copied from a template that every
//     migration was applied to once, and dropped when the test ends;
//   - Dgraph: everything dropped, data and schema, and the production schema
//     applied again, as at the server's boot;
//   - OpenSearch: every index and retention policy the earlier tests made
//     deleted;
//   - Redis: emptied.
//
// When a test ends, the project's clients reach nothing again, as when its
// own container went. The tests run one at a time (they share the project's
// package-level clients), so no two tests use a server at once. The
// containers go when the binary ends: TestMain stops them, and
// testcontainers' reaper removes them if the binary dies first.

import (
	"context"
	"sync"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// shared is a server the binary starts once, on first use. A start that
// failed leaves none, and the next test that asks starts one again.
type shared[T any] struct {
	mu   sync.Mutex
	srv  *T
	stop func()
}

// get is the server, started with start if there is none yet. start returns
// the server and how to stop it.
func (s *shared[T]) get(start func() (*T, func(), error)) (*T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		srv, stop, err := start()
		if err != nil {
			return nil, err
		}
		var once sync.Once
		s.srv, s.stop = srv, func() { once.Do(stop) }
		stops.add(s.stop)
	}
	return s.srv, nil
}

// current is the server if one is running, without starting one.
func (s *shared[T]) current() *T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv
}

// prepared is the server, made ready for a test by prepare (emptied of what
// the tests before left). A server that can't be made ready is stopped and a
// new one started, once: a server that died or wedged mid-run would
// otherwise fail every test after the one that broke it, where each test's
// own container failed only that test.
func (s *shared[T]) prepared(start func() (*T, func(), error), prepare func(*T) error) (*T, error) {
	srv, err := s.get(start)
	if err != nil {
		return nil, err
	}
	if err := prepare(srv); err == nil {
		return srv, nil
	}
	s.mu.Lock()
	if s.srv == srv {
		s.stop()
		s.srv, s.stop = nil, nil
	}
	s.mu.Unlock()
	if srv, err = s.get(start); err != nil {
		return nil, err
	}
	if err := prepare(srv); err != nil {
		return nil, err
	}
	return srv, nil
}

// stopList is the started servers' stops, in the order the servers started.
type stopList struct {
	mu  sync.Mutex
	fns []func()
}

var stops stopList

func (s *stopList) add(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fns = append(s.fns, fn)
}

// stopServers stops every server the binary started, the last started first.
// TestMain calls it once every test has run.
func stopServers() {
	stops.mu.Lock()
	fns := stops.fns
	stops.fns = nil
	stops.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// terminate removes a container, giving it 30 s.
func terminate(c testcontainers.Container) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Terminate(ctx)
}
