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
// hook's lifetime. Each call runs under mu and re-checks closed inside it, so
// close (which takes mu) waits out a call already in flight and no SQL can
// start once it returns: a hook goroutine or a retained Querier cannot write
// after admission commits or rolls back. close also cancels, synchronously,
// every context it handed out, so a row the hook left unscanned cannot hold
// the connection through cleanup, whatever context the hook passed.
type hookQuerier struct {
	conn    *sql.Conn
	mu      sync.Mutex
	closed  bool
	cancels []context.CancelFunc
}

// bind returns the context for one call. Callers hold mu. Once closed, the
// returned context is already cancelled, so the call fails without running.
func (h *hookQuerier) bind(ctx context.Context) context.Context {
	bound, cancel := context.WithCancel(ctx)
	if h.closed {
		cancel()
		return bound
	}
	h.cancels = append(h.cancels, cancel)
	return bound
}

func (h *hookQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn.ExecContext(h.bind(ctx), query, args...)
}

func (h *hookQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn.QueryRowContext(h.bind(ctx), query, args...)
}

// close ends the Querier: it waits for an in-flight call, refuses every later
// one and cancels every context handed out.
func (h *hookQuerier) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, cancel := range h.cancels {
		cancel()
	}
	h.cancels = nil
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
func runAdmittedHook(ctx context.Context, conn *sql.Conn, hook func(context.Context, Querier, string) error, id string) error {
	hookCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := &hookQuerier{conn: conn}
	defer q.close()
	return hook(hookCtx, q, id)
}
