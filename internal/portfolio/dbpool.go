package portfolio

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DBPool manages read-only database connections to child project DBs.
type DBPool struct {
	mu          sync.Mutex
	handles     map[string]*sql.DB
	busyTimeout time.Duration
}

// NewDBPool creates a pool for read-only child DB access.
func NewDBPool(busyTimeout time.Duration) *DBPool {
	if busyTimeout <= 0 {
		busyTimeout = 500 * time.Millisecond
	}
	return &DBPool{
		handles:     make(map[string]*sql.DB),
		busyTimeout: busyTimeout,
	}
}

// Get returns a read-only DB handle for a child project directory.
// Handles are cached and reused across poll cycles.
func (p *DBPool) Get(projectDir string) (*sql.DB, error) {
	// Enforce absolute paths
	if !filepath.IsAbs(projectDir) {
		return nil, fmt.Errorf("dbpool: project dir must be absolute: %q", projectDir)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if db, ok := p.handles[projectDir]; ok {
		return db, nil
	}

	dbPath := filepath.Join(projectDir, ".clavain", "intercore.db")
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout%%3D%d", dbPath, p.busyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbpool: open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	// Verify DB is readable
	var result int
	if err := db.QueryRow("SELECT 1").Scan(&result); err != nil {
		db.Close()
		return nil, fmt.Errorf("dbpool: verify %s: %w", dbPath, err)
	}

	p.handles[projectDir] = db
	return db, nil
}

// Close closes all cached DB handles.
func (p *DBPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, db := range p.handles {
		db.Close()
	}
	p.handles = make(map[string]*sql.DB)
}
