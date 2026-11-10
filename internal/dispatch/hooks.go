package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	conn     *sql.Conn
	mu       sync.Mutex
	closed   bool
	poisoned bool // a call was cancelled or interrupted; see finish
	cancels  []context.CancelFunc
	running  int // calls between begin and finish
	active   sync.WaitGroup
}

// errHookPoisoned is what a Querier answers once one of its calls was
// cancelled or interrupted. See finish for why every later call is refused.
var errHookPoisoned = errors.New("dispatch: an earlier query of this hook was cancelled or interrupted, so the admission transaction can no longer be trusted; the hook must return an error")

// begin registers one call. It returns the context to run it under and the
// function to call, with the call's error, when it has finished. Once closed or
// poisoned, the context is already cancelled, so the call fails without running.
func (h *hookQuerier) begin(ctx context.Context) (context.Context, func(error), bool) {
	bound, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.poisoned {
		cancel()
		return bound, func(error) {}, false
	}
	h.cancels = append(h.cancels, cancel)
	h.running++
	h.active.Add(1)
	return bound, func(err error) { h.finish(bound, err) }, true
}

// finish ends one call. A call whose context ended, or that failed as
// interrupted, may have had SQLite interrupt it, and SQLite answers an
// interrupted INSERT, UPDATE or DELETE inside a transaction by rolling the
// whole transaction back. Every later statement would then run in autocommit
// mode and persist outside the admission transaction. The same interrupt also
// stays pending on the connection until the pin is released (see pinConn), so
// later statements would fail at random. So the first such call poisons the
// Querier: every later call is refused and the hook is refused even if it
// returns nil. An interrupt can only be delivered while the call's context is
// done, so checking the context here never misses one; it may refuse a hook
// whose context ended just after its query completed, which is the safe side.
func (h *hookQuerier) finish(bound context.Context, err error) {
	h.mu.Lock()
	if bound.Err() != nil || wasInterrupted(err) {
		h.poisoned = true
	}
	h.running--
	h.mu.Unlock()
	h.active.Done()
}

func (h *hookQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	bound, finish, ok := h.begin(ctx)
	if !ok {
		return nil, errHookPoisonedOrClosed(h)
	}
	res, err := h.conn.ExecContext(bound, query, args...)
	finish(err)
	return res, err
}

func (h *hookQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	bound, finish, _ := h.begin(ctx)
	row := h.conn.QueryRowContext(bound, query, args...)
	finish(nil)
	return row
}

// errHookPoisonedOrClosed names why a call was refused without running.
func errHookPoisonedOrClosed(h *hookQuerier) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.poisoned && !h.closed {
		return errHookPoisoned
	}
	return context.Canceled
}

// wasInterrupted reports a driver error that is, or follows from, an interrupt.
func wasInterrupted(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "interrupted")
}

// close ends the Querier: it refuses every later call, cancels every context
// handed out, then waits for the calls still running to return. It reports how
// many calls were still running when it began, i.e. how many it interrupted.
//
// The wait always ends because the connection is pinned (see pinConn): the
// driver delivers one interrupt per call, and SQLite drops an interrupt that
// lands before it has started the statement, but with a statement held open the
// interrupt stays pending until every statement the hook started has ended.
func (h *hookQuerier) close() (interrupted int) {
	h.mu.Lock()
	h.closed = true
	interrupted = h.running
	cancels := h.cancels
	h.cancels = nil
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	h.active.Wait()
	return interrupted
}

// pinSQL is a query that returns more than one row, so it stays active after
// its first row has been read.
const pinSQL = `WITH RECURSIVE pin(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM pin LIMIT 3) SELECT x FROM pin`

// pinConn keeps one statement active on the connection, until the returned
// function is called. SQLite clears a pending interrupt when a statement starts
// while none is active, and the driver sends only one interrupt per call, so an
// interrupt that arrives between the driver arming it and SQLite starting the
// statement is lost and an unbounded hook query then runs to the end with
// nothing able to stop it. While one statement is active, the interrupt stays
// pending and every statement started meanwhile ends at once. The pin runs under
// no context, so nothing interrupts it, and it must be released before admission
// commits or rolls back.
func pinConn(ctx context.Context, conn *sql.Conn) (release func(), err error) {
	rows, err := conn.QueryContext(context.WithoutCancel(ctx), pinSQL)
	if err != nil {
		return nil, fmt.Errorf("dispatch: pin the admission connection: %w", err)
	}
	if !rows.Next() {
		err := rows.Err()
		rows.Close()
		return nil, fmt.Errorf("dispatch: pin the admission connection: no row (%v)", err)
	}
	return func() { rows.Close() }, nil
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
// reports it as an error and admission never commits. The same holds for a
// hook that returns nil after one of its queries was cancelled (see
// hookQuerier.finish), and the connection is pinned for the hook's lifetime so
// that interrupting a call can never be lost (see pinConn).
func runAdmittedHook(ctx context.Context, conn *sql.Conn, hook func(context.Context, Querier, string) error, id string) error {
	release, err := pinConn(ctx, conn)
	if err != nil {
		return err
	}
	defer release() // runs after the Querier is closed
	hookCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := &hookQuerier{conn: conn}
	closed := false
	defer func() {
		if !closed { // the hook panicked
			q.close()
		}
	}()
	err = hook(hookCtx, q, id)
	closed = true
	interrupted := q.close()
	if err != nil {
		return err
	}
	if interrupted > 0 {
		return fmt.Errorf("hook returned while %d of its queries were still running; they were interrupted", interrupted)
	}
	if q.poisoned {
		return errHookPoisoned
	}
	return nil
}

// errNoTransaction reports SQLite's answer to ROLLBACK when no transaction is
// open. The connection is then already clean: an interrupted write inside an
// explicit transaction rolls the whole transaction back by itself.
func errNoTransaction(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no transaction is active")
}
