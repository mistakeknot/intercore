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
)

// Querier is the database access an OnAdmitted hook gets. It is backed by the
// single connection that admission already owns, inside the admission
// transaction, so everything the hook writes commits or rolls back together
// with the dispatch row. The hook must use only this Querier: the store holds
// the only connection (SetMaxOpenConns(1)), so any nested Store call would
// block until the transaction ends, and the hook must never COMMIT or
// ROLLBACK.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// admissionQuerier is the same surface, used by the admission helpers.
type admissionQuerier = Querier

// hookQuerier narrows the admission connection to Querier, so a hook cannot
// recover the *sql.Conn by type assertion.
type hookQuerier struct{ conn *sql.Conn }

func (h hookQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return h.conn.ExecContext(ctx, query, args...)
}

func (h hookQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return h.conn.QueryRowContext(ctx, query, args...)
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
