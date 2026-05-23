package receipt

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// KeyStore returns active and historical signing keys per agent.
//
// canon §Key handling: one key per agent identity per rotation epoch. Active
// returns the current epoch's key (used for new signing); Get returns any
// past key by KeyID (used for verifying older receipts during the trust
// window).
type KeyStore interface {
	// Active returns the active KeyID and key bytes for an agent.
	// The KeyID format is "<agent_id>#<rotation_epoch>" — for example,
	// "sylveste://agent/hassease#2026-q2".
	Active(agentID string) (keyID string, key []byte, err error)
	// Get returns the key bytes for an arbitrary KeyID. Used by Verify
	// to validate receipts signed under prior rotation epochs.
	Get(keyID string) ([]byte, error)
}

// Sentinel errors from KeyStore implementations.
var (
	// ErrNoActiveKey is returned by Active when no key is registered for
	// the agent.
	ErrNoActiveKey = errors.New("no active signing key for agent")

	// ErrUnknownKey is returned by Get when the KeyID is not present.
	ErrUnknownKey = errors.New("unknown key id")
)

// MemKeyStore is an in-memory KeyStore used in tests and for v1 bootstrap.
// Production deployments use FileKeyStore.
//
// MemKeyStore is safe for concurrent use.
type MemKeyStore struct {
	mu     sync.RWMutex
	active map[string]string // agentID -> active keyID
	keys   map[string][]byte // keyID    -> key bytes
}

// NewMemKeyStore returns an empty in-memory KeyStore.
func NewMemKeyStore() *MemKeyStore {
	return &MemKeyStore{
		active: map[string]string{},
		keys:   map[string][]byte{},
	}
}

// Register installs key as the active key for agentID at the given epoch
// (e.g., "2026-q2") and returns the resulting KeyID. Subsequent calls for
// the same agentID rotate the active key; older epochs remain in the store
// for verify. A defensive copy of key is held.
func (m *MemKeyStore) Register(agentID, epoch string, key []byte) string {
	keyID := agentID + "#" + epoch
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[keyID] = append([]byte(nil), key...)
	m.active[agentID] = keyID
	return keyID
}

// GenerateAndRegister creates a fresh 256-bit key for agentID at epoch and
// installs it as active. canon §Key handling: v1 keys come from crypto/rand;
// v2 may move to HKDF-derived per-agent keys from a project-level secret.
func (m *MemKeyStore) GenerateAndRegister(agentID, epoch string) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return m.Register(agentID, epoch, key), nil
}

// Active implements KeyStore.
func (m *MemKeyStore) Active(agentID string) (string, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keyID, ok := m.active[agentID]
	if !ok {
		return "", nil, fmt.Errorf("%w: %s", ErrNoActiveKey, agentID)
	}
	return keyID, m.keys[keyID], nil
}

// Get implements KeyStore.
func (m *MemKeyStore) Get(keyID string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, keyID)
	}
	return key, nil
}

// FileKeyStore reads keys from a directory tree at .clavain/keys/receipts/
// per canon §Key handling: "plugin-local secret store at
// .clavain/keys/receipts/<agent>/<epoch>.key" with permissions 0600.
//
// agent_id URIs like "sylveste://agent/hassease" are reduced to filesystem
// segments by replacing "://" with "_": the URI maps deterministically to a
// directory under Root. The active epoch pointer lives at
// <agent-segment>/active (a single-line file containing the epoch string,
// e.g. "2026-q2").
type FileKeyStore struct {
	Root string
}

// Active implements KeyStore.
func (f *FileKeyStore) Active(agentID string) (string, []byte, error) {
	seg := agentSegment(agentID)
	activePath := filepath.Join(f.Root, seg, "active")
	epochBytes, err := os.ReadFile(activePath)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %s", ErrNoActiveKey, agentID)
	}
	epoch := strings.TrimSpace(string(epochBytes))
	keyID := agentID + "#" + epoch
	key, err := f.Get(keyID)
	if err != nil {
		return "", nil, err
	}
	return keyID, key, nil
}

// Get implements KeyStore.
func (f *FileKeyStore) Get(keyID string) ([]byte, error) {
	agentID, epoch, ok := strings.Cut(keyID, "#")
	if !ok {
		return nil, fmt.Errorf("%w: malformed key id %q", ErrUnknownKey, keyID)
	}
	path := filepath.Join(f.Root, agentSegment(agentID), epoch+".key")
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, keyID)
	}
	return key, nil
}

// agentSegment turns an agent_id URI into a filesystem-safe segment. The
// rule is intentionally minimal — strip the "://" separator only — so that
// the mapping is obvious and reversible by inspection.
func agentSegment(agentID string) string {
	return strings.ReplaceAll(agentID, "://", "_")
}
