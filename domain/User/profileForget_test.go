package domain

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/redis/go-redis/v9"
)

// panicOnLateDelete is a Redis client hook that answers forgetProfile's first
// forget (one person's two keys) without a server, and panics on the next
// delete: the forget it runs again after profileSettle.
type panicOnLateDelete struct{ deletes atomic.Int32 }

func (h *panicOnLateDelete) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *panicOnLateDelete) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "del" && h.deletes.Add(1) > 2 {
			panic("the cache went away under a late forget")
		}
		return nil
	}
}

func (h *panicOnLateDelete) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// lockedLog is a log sink the recovering goroutine writes and the test reads.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The second forget runs on the timer's goroutine, after the request that
// asked for it has ended. A panic there, such as a cache client torn down
// under it, would end the whole server, so it is recovered and logged. Without
// GoSafeNamed around it this test binary dies of the panic.
func TestASecondForgetThatPanicsDoesNotEndTheServer(t *testing.T) {
	logs := &lockedLog{}
	hook := &panicOnLateDelete{}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	client.AddHook(hook)
	savedLogger, savedClient, savedSettle := helpers.Logger, redisInit.RedisClient, profileSettle
	helpers.Logger = slog.New(slog.NewTextHandler(logs, nil))
	redisInit.RedisClient, profileSettle = client, time.Millisecond
	t.Cleanup(func() {
		helpers.Logger, redisInit.RedisClient, profileSettle = savedLogger, savedClient, savedSettle
		_ = client.Close()
	})

	forgetProfile(context.Background(), "a-person")

	// Waits for the log line, which the recovering goroutine writes last; the
	// bound only stops a broken build from hanging the run.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "GoSafeNamed[forgetProfile] recovered panic") {
		if time.Now().After(deadline) {
			t.Fatalf("no recovered panic was logged for the late forget; %d deletes were asked for; logs:\n%s", hook.deletes.Load(), logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := hook.deletes.Load(); got != 3 {
		t.Errorf("%d deletes were asked for; want 3 (two at once, then the late forget's first)", got)
	}
}

// A person's cached profile is forgotten in one place. forgetProfile is the
// invalidation that also outlasts a read in flight during the change; a Delete
// of either key anywhere else is a second, weaker one, and a change that used it
// could leave a demoted admin, or a deactivated account, served from the cache
// for its whole hour.
func TestTheProfileCacheIsForgottenInOnePlace(t *testing.T) {
	var where []string
	for _, root := range []string{"../../business", "../../controllers", "../../domain", "../../helpers",
		"../../middleware", "../../models", "../../services", "../../router"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !strings.HasPrefix(sel.Sel.Name, "Delete") {
						return true
					}
					for _, arg := range call.Args {
						if s, ok := arg.(*ast.SelectorExpr); ok && (s.Sel.Name == "UserProfile" || s.Sel.Name == "UserDgraphProfile") {
							where = append(where, fn.Name.Name+" ("+fset.Position(call.Pos()).String()+")")
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(where) == 0 {
		t.Fatal("found no deletion of the profile cache at all; this check is looking in the wrong place")
	}
	for _, w := range where {
		if !strings.HasPrefix(w, "forgetProfile ") {
			t.Errorf("the profile cache is deleted outside forgetProfile, in %s; call forgetProfile instead", w)
		}
	}
}
