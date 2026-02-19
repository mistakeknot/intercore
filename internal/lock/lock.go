package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultBaseDir   = "/tmp/intercore/locks"
	DefaultStaleAge  = 5 * time.Second
	DefaultMaxWait   = time.Second
	DefaultRetryWait = 100 * time.Millisecond
)

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
}

// Manager manages filesystem-based locks under a base directory.
type Manager struct {
	BaseDir  string
	StaleAge time.Duration
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
func (m *Manager) Acquire(_ context.Context, name, scope, owner string, maxWait time.Duration) error {
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}

	ld, err := m.lockDir(name, scope)
	if err != nil {
		return fmt.Errorf("lock acquire: %w", err)
	}

	// Ensure parent dir exists (name level).
	if err := os.MkdirAll(filepath.Dir(ld), 0700); err != nil {
		return fmt.Errorf("lock acquire: mkdir parent: %w", err)
	}

	deadline := time.Now().Add(maxWait)
	for {
		// Atomic acquire attempt.
		err := os.Mkdir(ld, 0700)
		if err == nil {
			// Lock acquired — write owner metadata.
			if werr := writeOwnerFile(ld, owner); werr != nil {
				// Failed to write metadata — release the lock dir.
				// The dir might already be gone (concurrent stale-break).
				os.Remove(ld)
				// Don't error — retry the acquire loop.
				continue
			}
			return nil
		}

		if !os.IsExist(err) {
			return fmt.Errorf("lock acquire: mkdir: %w", err)
		}

		// Lock dir exists — check if stale.
		if m.tryBreakStale(ld) {
			continue // Stale lock broken, retry immediately.
		}

		if time.Now().After(deadline) {
			return ErrTimeout
		}
		time.Sleep(DefaultRetryWait)
	}
}

// Release releases a lock, verifying the caller is the owner.
func (m *Manager) Release(_ context.Context, name, scope, owner string) error {
	ld, err := m.lockDir(name, scope)
	if err != nil {
		return fmt.Errorf("lock release: %w", err)
	}
	of := ownerFilePath(ld)

	meta, err := readOwnerFile(of)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		// owner.json unreadable but dir may exist — check dir existence.
		if _, serr := os.Stat(ld); os.IsNotExist(serr) {
			return ErrNotFound
		}
		// Dir exists but owner.json corrupted/missing — don't silently remove.
		// Delegate cleanup to ic lock clean.
		return ErrNotFound
	}

	if meta.Owner != owner {
		return ErrNotOwner
	}

	os.Remove(of)
	if err := os.Remove(ld); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lock release: rmdir: %w", err)
	}
	return nil
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
		// PID-liveness check: only remove if the process is dead.
		if l.PID > 0 && pidAlive(l.PID) {
			continue // Process is still running — skip.
		}

		ld := filepath.Join(m.BaseDir, l.Name, l.Scope)
		of := ownerFilePath(ld)
		os.Remove(of)
		if err := os.Remove(ld); err == nil {
			removed++
		}
	}
	return removed, nil
}

// writeOwnerFile writes the owner metadata file inside the lock dir.
func writeOwnerFile(lockDir, owner string) error {
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
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	return os.WriteFile(ownerFilePath(lockDir), data, 0600)
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

// tryBreakStale checks if a lock is stale and attempts to break it.
// Returns true if the lock was broken (caller should retry acquire).
// Uses rename-then-remove pattern: renames owner.json to a unique temp name
// to atomically claim the right to break, preventing dual-ownership.
func (m *Manager) tryBreakStale(ld string) bool {
	of := ownerFilePath(ld)
	meta, err := readOwnerFile(of)
	if err != nil {
		// Can't read owner — might be mid-creation. Don't break.
		return false
	}

	created := time.Unix(meta.Created, 0)
	if time.Since(created) < m.StaleAge {
		return false // Not stale yet.
	}

	// Stale lock — attempt to break atomically.
	// Rename owner.json to a unique temp name; only one goroutine succeeds.
	breaking := of + fmt.Sprintf(".breaking.%d", time.Now().UnixNano())
	if err := os.Rename(of, breaking); err != nil {
		return false // Another breaker already renamed — let them handle it.
	}
	// We won the rename race — clean up.
	os.Remove(breaking)
	err = os.Remove(ld)
	return err == nil
}

// pidAlive checks if a process with the given PID is running.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
