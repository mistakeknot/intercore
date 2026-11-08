package lock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseDir   = "/tmp/intercore/locks"
	DefaultStaleAge  = 5 * time.Second
	DefaultMaxWait   = time.Second
	DefaultRetryWait = 100 * time.Millisecond

	// releaseClaimWait bounds how long Release waits for a cleaner that holds
	// the per-lock claim.
	releaseClaimWait = 2 * time.Second
)

// staleClaimAge is the bound after which a cleaner claim (cleaner.json) or a
// pending record (.pending.*) is abandoned even if its pid still looks alive
// (pid reuse, a process stopped for good). A claim or pending file lives for
// microseconds in normal operation.
var staleClaimAge = 30 * time.Second

var (
	ErrTimeout  = errors.New("lock acquire timed out")
	ErrNotOwner = errors.New("not lock owner")
	ErrNotFound = errors.New("lock not found")
	ErrBadName  = errors.New("invalid lock name or scope")
)

// Lock represents an active lock with its metadata.
type Lock struct {
	Name    string    `json:"name"`
	Scope   string    `json:"scope"`
	Owner   string    `json:"owner"`
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	Created time.Time `json:"created"`
}

// ownerMeta is the JSON structure written to owner.json inside the lock dir.
type ownerMeta struct {
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Owner   string `json:"owner"`
	Created int64  `json:"created"`
	// Token identifies one acquisition. It is absent from records written by
	// older clients; readers must accept records without it.
	Token string `json:"token,omitempty"`
}

// claimMeta is the JSON structure of cleaner.json, the per-lock claim that
// serializes cleaners (and Release) against each other. The bash fallback in
// hooks/lib-intercore.sh writes {"pid":N,"created":T}; host and token are
// optional additions.
type claimMeta struct {
	PID     int    `json:"pid"`
	Host    string `json:"host,omitempty"`
	Created int64  `json:"created"`
	Token   string `json:"token,omitempty"`
}

// Manager manages filesystem-based locks under a base directory.
type Manager struct {
	BaseDir  string
	StaleAge time.Duration

	mu   sync.Mutex
	held map[string]string // lock dir + "\x00" + owner -> acquisition token
}

// NewManager creates a lock manager. Empty baseDir defaults to DefaultBaseDir.
func NewManager(baseDir string) *Manager {
	if baseDir == "" {
		baseDir = DefaultBaseDir
	}
	return &Manager{
		BaseDir:  baseDir,
		StaleAge: DefaultStaleAge,
	}
}

// validateComponent rejects lock name/scope components that could escape BaseDir.
func validateComponent(s string) error {
	if s == "" || s == "." || s == ".." ||
		strings.ContainsAny(s, "/\\") ||
		strings.Contains(s, "..") {
		return ErrBadName
	}
	return nil
}

// lockDir returns the filesystem path for a lock: <baseDir>/<name>/<scope>.
func (m *Manager) lockDir(name, scope string) (string, error) {
	if err := validateComponent(name); err != nil {
		return "", fmt.Errorf("lock name: %w", err)
	}
	if err := validateComponent(scope); err != nil {
		return "", fmt.Errorf("lock scope: %w", err)
	}
	ld := filepath.Join(m.BaseDir, name, scope)
	// Containment check: resolved path must be under BaseDir.
	abs, err := filepath.Abs(ld)
	if err != nil {
		return "", fmt.Errorf("lock dir: %w", err)
	}
	base, err := filepath.Abs(m.BaseDir)
	if err != nil {
		return "", fmt.Errorf("lock dir: %w", err)
	}
	if !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", ErrBadName
	}
	return ld, nil
}

// ownerFilePath returns the path to owner.json inside a lock dir.
func ownerFilePath(lockDir string) string {
	return filepath.Join(lockDir, "owner.json")
}

// Acquire attempts to acquire a named lock with spin-wait.
// Owner identifies the caller (typically "PID:hostname").
// maxWait controls how long to spin before returning ErrTimeout.
//
// The acquisition token is remembered by this Manager, so a later Release of
// the same lock by the same owner on the same Manager checks it. Callers that
// release from another process use AcquireToken and ReleaseToken.
func (m *Manager) Acquire(ctx context.Context, name, scope, owner string, maxWait time.Duration) error {
	token, err := m.AcquireToken(ctx, name, scope, owner, maxWait)
	if err != nil {
		return err
	}
	if ld, lerr := m.lockDir(name, scope); lerr == nil {
		m.mu.Lock()
		if m.held == nil {
			m.held = make(map[string]string)
		}
		m.held[heldKey(ld, owner)] = token
		m.mu.Unlock()
	}
	return nil
}

func heldKey(ld, owner string) string { return ld + "\x00" + owner }

// AcquireToken is Acquire that also returns the acquisition token written into
// the owner record. Pass it to ReleaseToken so a delayed release cannot remove
// a successor's record that carries the same owner string.
func (m *Manager) AcquireToken(_ context.Context, name, scope, owner string, maxWait time.Duration) (string, error) {
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}

	ld, err := m.lockDir(name, scope)
	if err != nil {
		return "", fmt.Errorf("lock acquire: %w", err)
	}

	// Ensure parent dir exists (name level).
	if err := os.MkdirAll(filepath.Dir(ld), 0700); err != nil {
		return "", fmt.Errorf("lock acquire: mkdir parent: %w", err)
	}

	deadline := time.Now().Add(maxWait)
	for {
		// Atomic acquire attempt.
		err := os.Mkdir(ld, 0700)
		if err == nil {
			// Remember which directory this attempt created: it is the only one
			// this attempt may remove.
			created, _ := os.Lstat(ld)
			if afterMkdirHook != nil {
				afterMkdirHook(ld)
			}
			token, werr := publishOwner(ld, owner)
			if werr == nil {
				return token, nil
			}
			if errors.Is(werr, fs.ErrExist) {
				// Another client (the bash fallback in hooks/lib-intercore.sh,
				// or a mkdir that reports success on an existing directory)
				// already published owner.json. The directory is theirs: leave
				// it and owner.json alone and treat this as contention.
				if err := waitOrTimeout(deadline, ErrTimeout); err != nil {
					return "", err
				}
				continue
			}
			// The record could not be written. Remove only the directory this
			// attempt created, and only if it is still empty; never a record.
			removeCreatedDir(ld, created)
			// Retry, but only until the deadline: a persistent failure must
			// surface as an error, not loop or succeed after the wait is over.
			if err := waitOrTimeout(deadline, fmt.Errorf("lock acquire: write owner: %w", werr)); err != nil {
				return "", err
			}
			continue
		}

		if !os.IsExist(err) {
			return "", fmt.Errorf("lock acquire: mkdir: %w", err)
		}

		// Lock dir exists — check if stale.
		if m.tryBreakStale(ld) {
			// Stale lock broken. Breaking can take a while (it checks the
			// owner's liveness), so look at the deadline before retrying.
			if time.Now().After(deadline) {
				return "", ErrTimeout
			}
			continue
		}

		if err := waitOrTimeout(deadline, ErrTimeout); err != nil {
			return "", err
		}
	}
}

// waitOrTimeout sleeps for one retry interval, never past the deadline, and
// returns failure if the deadline has passed before or after the sleep. The
// caller must not attempt the lock again after a non-nil return.
func waitOrTimeout(deadline time.Time, failure error) error {
	if !time.Now().Before(deadline) {
		return failure
	}
	d := DefaultRetryWait
	if rem := time.Until(deadline); rem < d {
		d = rem
	}
	if d > 0 {
		time.Sleep(d)
	}
	if !time.Now().Before(deadline) {
		return failure
	}
	return nil
}

// removeCreatedDir removes lockDir only if it is still the directory this
// attempt created (same file as created) and is empty (rmdir refuses otherwise).
func removeCreatedDir(lockDir string, created os.FileInfo) {
	if created == nil {
		return
	}
	cur, err := os.Lstat(lockDir)
	if err != nil || !os.SameFile(created, cur) {
		return
	}
	_ = os.Remove(lockDir)
}

// Release releases a lock, verifying the caller is the owner. If this Manager
// acquired the lock for owner, the acquisition token is verified as well.
func (m *Manager) Release(ctx context.Context, name, scope, owner string) error {
	ld, err := m.lockDir(name, scope)
	if err != nil {
		return fmt.Errorf("lock release: %w", err)
	}
	key := heldKey(ld, owner)
	m.mu.Lock()
	token := m.held[key]
	m.mu.Unlock()

	err = m.ReleaseToken(ctx, name, scope, owner, token)
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotOwner) {
		m.mu.Lock()
		delete(m.held, key)
		m.mu.Unlock()
	}
	return err
}

// ReleaseToken releases a lock for owner. A non-empty token must match the
// token in the owner record; records written without a token (older clients)
// are matched on the owner string alone, and an empty token skips the check.
//
// The record is removed under the per-lock cleaner claim, after reading it
// again, so a cleaner that judged the record dead cannot race the removal.
func (m *Manager) ReleaseToken(_ context.Context, name, scope, owner, token string) error {
	ld, err := m.lockDir(name, scope)
	if err != nil {
		return fmt.Errorf("lock release: %w", err)
	}
	of := ownerFilePath(ld)

	rec, err := os.ReadFile(of)
	if err != nil {
		// Missing, or unreadable with the dir possibly present: never remove
		// what cannot be proven ours. Delegate cleanup to ic lock clean.
		return ErrNotFound
	}
	var meta ownerMeta
	if err := json.Unmarshal(rec, &meta); err != nil {
		return ErrNotFound
	}
	if !ownedBy(&meta, owner, token) {
		return ErrNotOwner
	}

	c, ok := m.takeClaimWait(ld, releaseClaimWait)
	if !ok {
		return fmt.Errorf("lock release: lock busy (cleaner claim held)")
	}
	cur, rerr := os.ReadFile(of)
	if rerr != nil || !bytes.Equal(cur, rec) {
		dropClaim(c)
		if rerr != nil && errors.Is(rerr, fs.ErrNotExist) {
			return ErrNotFound
		}
		return ErrNotOwner // the record changed hands since we checked it
	}
	rmErr := os.Remove(of)
	dropClaim(c)
	if rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("lock release: remove owner: %w", rmErr)
	}
	// Only an empty directory goes: a successor mid-publish has a file in it.
	if err := os.Remove(ld); err != nil && !errors.Is(err, fs.ErrNotExist) && !isNotEmpty(err) {
		return fmt.Errorf("lock release: rmdir: %w", err)
	}
	return nil
}

// ownedBy reports whether meta is the record of (owner, token). An empty
// token, or a record without one, falls back to the owner string alone.
func ownedBy(meta *ownerMeta, owner, token string) bool {
	if meta.Owner != owner {
		return false
	}
	return token == "" || meta.Token == "" || meta.Token == token
}

// List returns all active locks.
func (m *Manager) List(_ context.Context) ([]Lock, error) {
	names, err := os.ReadDir(m.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lock list: %w", err)
	}

	var locks []Lock
	for _, nameEntry := range names {
		if !nameEntry.IsDir() {
			continue
		}
		scopes, err := os.ReadDir(filepath.Join(m.BaseDir, nameEntry.Name()))
		if err != nil {
			continue
		}
		for _, scopeEntry := range scopes {
			if !scopeEntry.IsDir() {
				continue
			}
			ld := filepath.Join(m.BaseDir, nameEntry.Name(), scopeEntry.Name())
			meta, err := readOwnerFile(ownerFilePath(ld))
			if err != nil {
				// Lock dir exists but no readable owner.json — include with empty metadata.
				locks = append(locks, Lock{
					Name:  nameEntry.Name(),
					Scope: scopeEntry.Name(),
				})
				continue
			}
			locks = append(locks, Lock{
				Name:    nameEntry.Name(),
				Scope:   scopeEntry.Name(),
				Owner:   meta.Owner,
				PID:     meta.PID,
				Host:    meta.Host,
				Created: time.Unix(meta.Created, 0),
			})
		}
	}
	return locks, nil
}

// Stale returns locks older than maxAge (by owner.json created timestamp).
// Locks with zero Created (missing owner.json) are also considered stale
// if their lock directory is older than maxAge.
func (m *Manager) Stale(_ context.Context, maxAge time.Duration) ([]Lock, error) {
	all, err := m.List(context.Background())
	if err != nil {
		return nil, err
	}

	threshold := time.Now().Add(-maxAge)
	var stale []Lock
	for _, l := range all {
		if !l.Created.IsZero() && l.Created.Before(threshold) {
			stale = append(stale, l)
			continue
		}
		// Ghost lock (no owner.json) — use dir mtime as fallback age.
		if l.Created.IsZero() {
			ld := filepath.Join(m.BaseDir, l.Name, l.Scope)
			if info, err := os.Stat(ld); err == nil && info.ModTime().Before(threshold) {
				stale = append(stale, l)
			}
		}
	}
	return stale, nil
}

// Clean removes stale locks whose owning PID is dead.
// Returns the number of locks removed.
func (m *Manager) Clean(_ context.Context, maxAge time.Duration) (int, error) {
	stale, err := m.Stale(context.Background(), maxAge)
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, l := range stale {
		ld := filepath.Join(m.BaseDir, l.Name, l.Scope)
		// breakDead applies the liveness check, the cleaner claim and the
		// byte-for-byte revalidation shared with Acquire's stale break.
		m.breakDead(ld, maxAge)
		if _, err := os.Lstat(ld); errors.Is(err, fs.ErrNotExist) {
			removed++
		}
	}
	return removed, nil
}

// Test seams. All are nil/identity in production.
var (
	// afterMkdirHook runs between a successful Mkdir and the owner write, so
	// tests can interleave another client deterministically.
	afterMkdirHook func(lockDir string)
	// afterClaimHook runs once a cleaner holds the claim, before it
	// revalidates the owner record.
	afterClaimHook func(lockDir string)
	// livenessCheck decides whether a recorded pid is alive.
	livenessCheck = func(pid int) bool { return pidAlive(pid) }
	// pendingWrite and pendingClose write and close the pending record file.
	pendingWrite = func(f *os.File, b []byte) error { _, err := f.Write(b); return err }
	pendingClose = func(f *os.File) error { return f.Close() }
)

// newToken returns a unique, shell- and JSON-safe identifier.
func newToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d.%d.%s", os.Getpid(), time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

// publish creates dest holding data, all at once, and fails with an error
// satisfying errors.Is(err, fs.ErrExist) if dest exists. The data is written to
// a unique pending file in the same directory and linked into place with
// link(2), which is atomic and never replaces an existing name, so a reader
// never sees a partial record. The pending name is ".pending.<pid>.<ns>.<rand>",
// the form the bash fallback parses to tell whether the writer is alive. The
// pending file is removed on every path: its name is unique to this call, so
// removing it cannot touch anybody else's file.
func publish(dest string, data []byte) error {
	pending := filepath.Join(filepath.Dir(dest), ".pending."+newToken())
	f, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := pendingWrite(f, data); err != nil {
		f.Close()
		os.Remove(pending)
		return err
	}
	if err := pendingClose(f); err != nil {
		// A record whose close failed may be incomplete; do not publish it.
		os.Remove(pending)
		return err
	}
	err = os.Link(pending, dest)
	os.Remove(pending)
	return err
}

// publishOwner publishes the owner record for owner in lockDir and returns the
// acquisition token it carries. The error satisfies errors.Is(err, fs.ErrExist)
// if another client already published owner.json.
func publishOwner(lockDir, owner string) (string, error) {
	pid := os.Getpid()
	host, _ := os.Hostname()

	// Parse PID from owner string if it has "PID:host" format.
	if parts := strings.SplitN(owner, ":", 2); len(parts) == 2 {
		if p, err := strconv.Atoi(parts[0]); err == nil {
			pid = p
		}
		host = parts[1]
	}

	meta := ownerMeta{
		PID:     pid,
		Host:    host,
		Owner:   owner,
		Created: time.Now().Unix(),
		Token:   newToken(),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	if err := publish(ownerFilePath(lockDir), data); err != nil {
		return "", err
	}
	return meta.Token, nil
}

// writeOwnerFile creates the owner metadata file inside the lock dir. The
// create is exclusive: if owner.json already exists another client has claimed
// the directory and the error satisfies errors.Is(err, fs.ErrExist).
func writeOwnerFile(lockDir, owner string) error {
	_, err := publishOwner(lockDir, owner)
	return err
}

// readOwnerFile reads the owner metadata from a lock dir's owner.json.
func readOwnerFile(ownerFile string) (*ownerMeta, error) {
	data, err := os.ReadFile(ownerFile)
	if err != nil {
		return nil, err
	}
	var meta ownerMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func isNotEmpty(err error) bool {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		s := pe.Err.Error()
		return strings.Contains(s, "not empty")
	}
	return false
}

// --- Cleaning: one protocol for Clean, Acquire's stale break and Release -------
//
// A record is never moved or removed on the strength of an earlier look, because
// its pathname can be reused the moment it is gone. Cleaners are serialized per
// lock by cleaner.json, published like owner.json (complete and exclusively),
// the same claim the bash fallback in hooks/lib-intercore.sh takes. Under the
// claim the record is read again and removed only if it is byte-for-byte the
// record that was judged dead; nothing else removes a dead owner's record, so it
// cannot change between that re-read and the removal. Only the cleaner's own
// claim is removed afterwards, and only an empty directory is removed.

// claim is a held cleaner.json.
type claim struct {
	path string
	data []byte
}

// takeClaim publishes this process's claim on ld. An existing claim blocks it
// unless it is abandoned (its cleaner is dead, or it is older than
// staleClaimAge); an abandoned claim is taken over.
func (m *Manager) takeClaim(ld string) (*claim, bool) {
	cf := filepath.Join(ld, "cleaner.json")
	host, _ := os.Hostname()
	data, err := json.Marshal(claimMeta{PID: os.Getpid(), Host: host, Created: time.Now().Unix(), Token: newToken()})
	if err != nil {
		return nil, false
	}
	err = publish(cf, data)
	if err == nil {
		return &claim{cf, data}, true
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, false
	}
	cur, rerr := os.ReadFile(cf)
	if rerr != nil || !claimAbandoned(cf, cur) {
		return nil, false // a live cleaner holds it (or it just went away: retry later)
	}
	if !removeExactly(cf, cur) {
		return nil, false
	}
	if err := publish(cf, data); err != nil {
		return nil, false
	}
	return &claim{cf, data}, true
}

// takeClaimWait is takeClaim retried until wait has elapsed.
func (m *Manager) takeClaimWait(ld string, wait time.Duration) (*claim, bool) {
	deadline := time.Now().Add(wait)
	for {
		if c, ok := m.takeClaim(ld); ok {
			return c, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// dropClaim removes the claim c, but only if cleaner.json still holds the bytes
// c published; a claim taken over from us is not ours to remove.
func dropClaim(c *claim) {
	if cur, err := os.ReadFile(c.path); err == nil && bytes.Equal(cur, c.data) {
		_ = os.Remove(c.path)
	}
}

// claimAbandoned reports whether the claim file holding cur belongs to a
// cleaner that is gone: older than staleClaimAge, or on this host with a dead
// pid. An unparsable claim is judged by the file's age alone.
func claimAbandoned(path string, cur []byte) bool {
	var cm claimMeta
	if err := json.Unmarshal(cur, &cm); err != nil {
		info, serr := os.Stat(path)
		return serr == nil && time.Since(info.ModTime()) > staleClaimAge
	}
	if time.Since(time.Unix(cm.Created, 0)) > staleClaimAge {
		return true
	}
	host, _ := os.Hostname()
	if cm.PID > 0 && (cm.Host == "" || cm.Host == host) && !livenessCheck(cm.PID) {
		return true
	}
	return false
}

// removeExactly removes path only if it holds want. It renames the file to a
// unique name first (atomic: only one caller wins), checks what it got, and puts
// it back with link(2) if it was not the file it meant to remove, so a
// successor's claim is not destroyed by a delayed cleaner.
func removeExactly(path string, want []byte) bool {
	tmp := path + ".stale." + newToken()
	if err := os.Rename(path, tmp); err != nil {
		return false
	}
	got, err := os.ReadFile(tmp)
	if err == nil && bytes.Equal(got, want) {
		os.Remove(tmp)
		return true
	}
	_ = os.Link(tmp, path) // restore; EEXIST means a newer claim already took its place
	os.Remove(tmp)
	return false
}

// dropStalePendings removes pending records of writers that are gone (dead pid,
// or older than staleClaimAge) so they cannot pin the directory. It returns how
// many it removed and how many pending files remain.
func dropStalePendings(ld string) (dropped, live int) {
	entries, err := os.ReadDir(ld)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".pending.") {
			continue
		}
		pid, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, ".pending."), ".", 2)[0])
		dead := pid <= 0 || !livenessCheck(pid)
		if !dead {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleClaimAge {
				dead = true
			}
		}
		if dead {
			if os.Remove(filepath.Join(ld, name)) == nil {
				dropped++
				continue
			}
		}
		live++
	}
	return dropped, live
}

// breakDead removes the lock at ld if it is stale (older than maxAge) and its
// owner is dead, or if it is an abandoned, ownerless directory. It reports
// whether it removed something.
//
// Left alone, deliberately (a stranded lock is acceptable, two holders are not):
//   - a record that cannot be parsed (empty, or being written by a client that
//     does not publish atomically): a half-written record is not a dead owner;
//   - a claim held by a live cleaner.
func (m *Manager) breakDead(ld string, maxAge time.Duration) bool {
	info, err := os.Lstat(ld)
	if err != nil {
		return false
	}
	dirMod := info.ModTime() // before cleaning touches the directory
	dropped, live := dropStalePendings(ld)

	of := ownerFilePath(ld)
	rec, err := os.ReadFile(of)
	if errors.Is(err, fs.ErrNotExist) {
		return reclaimOwnerless(ld, dirMod, maxAge, dropped, live)
	}
	if err != nil {
		return false
	}
	var meta ownerMeta
	if json.Unmarshal(rec, &meta) != nil {
		return false
	}
	if time.Since(time.Unix(meta.Created, 0)) < maxAge {
		return false // not stale yet
	}
	// A stale-aged lock whose owner process is still running is a live lock:
	// age alone never justifies breaking it.
	if meta.PID > 0 && livenessCheck(meta.PID) {
		return false
	}

	c, ok := m.takeClaim(ld)
	if !ok {
		return false
	}
	if afterClaimHook != nil {
		afterClaimHook(ld)
	}
	removed := false
	if cur, err := os.ReadFile(of); err == nil && bytes.Equal(cur, rec) {
		removed = os.Remove(of) == nil
	}
	dropClaim(c)
	rmdirErr := os.Remove(ld) // rmdir: refuses unless empty
	return removed || rmdirErr == nil
}

// reclaimOwnerless removes a directory that has no owner record: a ghost left
// by a crashed acquirer, a directory pinned only by dead writers' pending
// files, or one holding only an abandoned cleaner claim.
func reclaimOwnerless(ld string, dirMod time.Time, maxAge time.Duration, dropped, live int) bool {
	if live > 0 {
		return false // a writer is mid-publish
	}
	abandoned := dropped > 0
	cf := filepath.Join(ld, "cleaner.json")
	if cur, err := os.ReadFile(cf); err == nil {
		if !claimAbandoned(cf, cur) || !removeExactly(cf, cur) {
			return false // a live cleaner is mid-way
		}
		abandoned = true
	}
	if !abandoned && time.Since(dirMod) < maxAge {
		return false
	}
	return os.Remove(ld) == nil
}

// tryBreakStale checks if a lock is stale and attempts to break it.
// Returns true if something was removed (caller should retry acquire).
func (m *Manager) tryBreakStale(ld string) bool {
	return m.breakDead(ld, m.StaleAge)
}
