package dispatch

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Querier is the database access an OnAdmitted hook gets. It is backed by the
// single connection that admission already owns, inside the admission
// transaction, so everything the hook writes commits or rolls back together
// with the dispatch row. The hook must use only this Querier: the store holds
// the only connection (SetMaxOpenConns(1)), so any nested Store call would
// block until the transaction ends, and the hook must never COMMIT or
// ROLLBACK. The context the hook receives ends when the hook returns, so a
// query result must be read before the hook returns, whatever context the
// hook passes to it.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// admissionQuerier is the same surface, used by the admission helpers.
type admissionQuerier = Querier

// hookQuerier narrows the admission connection to Querier, so a hook cannot
// recover the *sql.Conn by type assertion, and it bounds the Querier to the
// hook's lifetime. A call registers under mu (a short critical section, never
// held across SQL) and is refused once closed is set. close sets closed and
// cancels every context the Querier handed out before it waits for the calls
// already running, so a long query a hook goroutine started is interrupted
// rather than waited on, and when close returns no hook SQL is running and none
// can start: nothing the hook began can write after admission commits or rolls
// back. The cancelled contexts also release any row the hook left unscanned, so
// it cannot hold the connection through cleanup, whatever context the hook
// passed.
type hookQuerier struct {
	conn      *sql.Conn
	mu        sync.Mutex
	closed    bool
	cancels   []context.CancelFunc
	running   int       // calls between begin and done
	lastBegin time.Time // when the latest call began
	active    sync.WaitGroup
}

// begin registers one call. It returns the context to run it under and the
// function to call when it has finished. Once closed, the context is already
// cancelled, so the call fails without running.
func (h *hookQuerier) begin(ctx context.Context) (context.Context, func()) {
	bound, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		cancel()
		return bound, func() {}
	}
	h.cancels = append(h.cancels, cancel)
	h.running++
	h.lastBegin = time.Now()
	h.active.Add(1)
	return bound, func() {
		h.mu.Lock()
		h.running--
		h.mu.Unlock()
		h.active.Done()
	}
}

func (h *hookQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	bound, done := h.begin(ctx)
	defer done()
	return h.conn.ExecContext(bound, query, args...)
}

func (h *hookQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	bound, done := h.begin(ctx)
	defer done()
	return h.conn.QueryRowContext(bound, query, args...)
}

// interruptGrace is how long a call gets to start in the driver before close
// interrupts it.
const interruptGrace = 50 * time.Millisecond

// close ends the Querier: it refuses every later call, cancels every context
// handed out, then waits for the calls still running to return. It reports how
// many calls were still running when it began, i.e. how many it interrupted.
func (h *hookQuerier) close() (interrupted int) {
	h.mu.Lock()
	h.closed = true
	interrupted = h.running
	cancels := h.cancels
	h.cancels = nil
	lastBegin := h.lastBegin
	h.mu.Unlock()
	if interrupted > 0 {
		// The driver's interrupt is lost when it lands before SQLite has started
		// the statement, and the statement then runs to the end on its own. Let
		// every call that is still running get that far first. Only a hook that
		// returned with a query running pays this wait, and that is an error.
		if wait := interruptGrace - time.Since(lastBegin); wait > 0 {
			time.Sleep(wait)
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
	h.active.Wait()
	return interrupted
}

// reservedEnv are the identity variables Spawn sets itself after admission.
// A caller can never supply them: the dispatch ID is the attempt identity and
// the worker cannot be handed another.
var reservedEnv = map[string]bool{
	"IC_RUN_ID": true, "IC_DISPATCH_ID": true, "CLAVAIN_DISPATCH_ID": true,
	"IC_DISPATCH_ATTEMPT": true, "IC_PROMPT_HASH": true,
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateExtraEnv(env map[string]string) error {
	for k, v := range env {
		if !envName.MatchString(k) {
			return &InputError{fmt.Sprintf("extra_env: invalid variable name %q", k)}
		}
		if reservedEnv[k] {
			return &InputError{fmt.Sprintf("extra_env: %s is set by spawn and cannot be overridden", k)}
		}
		if strings.ContainsRune(v, 0) {
			return &InputError{fmt.Sprintf("extra_env: value of %s contains NUL", k)}
		}
	}
	return nil
}

// extraEnvList renders ExtraEnv as KEY=VALUE in a stable order.
func extraEnvList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// resolveStrictDispatchSH is the strict-mode counterpart of resolveDispatchSH:
// exactly the given path, whether or not it exists. There is no
// CLAVAIN_DISPATCH_SH, no monorepo search and no fallback; existence is
// checked by checkStrictExecutor immediately before Start.
func resolveStrictDispatchSH(explicit string) string { return explicit }

// checkStrictExecutor verifies the claimed executor is a regular file with an
// execute bit. Spawn runs it just before cmd.Start().
func checkStrictExecutor(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

func validateStrictExecutor(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return &InputError{"strict executor requires an absolute dispatch_sh path"}
	}
	return nil
}

// runAdmittedHook calls the hook with a Querier and a context that both end
// when the hook returns or panics. The Querier is closed before this returns,
// so nothing the hook started can touch the connection once admission goes on
// to commit or roll back.
//
// A hook that returns nil while one of its calls is still running has not
// finished its work, and the interrupt that ends the call can end the
// admission transaction itself: SQLite rolls an explicit transaction back when
// it interrupts an INSERT, UPDATE or DELETE inside it. Whether the write was
// cut short or finished just in time would then depend on timing, so this
// reports it as an error and admission never commits.
func runAdmittedHook(ctx context.Context, conn *sql.Conn, hook func(context.Context, Querier, string) error, id string) error {
	hookCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := &hookQuerier{conn: conn}
	closed := false
	defer func() {
		if !closed { // the hook panicked
			q.close()
		}
	}()
	err := hook(hookCtx, q, id)
	closed = true
	interrupted := q.close()
	if err != nil {
		return err
	}
	if interrupted > 0 {
		return fmt.Errorf("hook returned while %d of its queries were still running; they were interrupted", interrupted)
	}
	return nil
}

// errNoTransaction reports SQLite's answer to ROLLBACK when no transaction is
// open. The connection is then already clean: an interrupted write inside an
// explicit transaction rolls the whole transaction back by itself.
func errNoTransaction(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no transaction is active")
}
