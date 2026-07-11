// Package runtimeproof defines and verifies the runtime-evidence/v1 close receipt.
package runtimeproof

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	SchemaVersion           = 1
	DefaultMaxReceiptBytes  = int64(256 << 10)
	DefaultMaxArtifactBytes = int64(512 << 20)
	DefaultMaxAge           = 24 * time.Hour
	gitResolveTimeout       = 2 * time.Second
)

// State is the fail-closed outcome vocabulary used by runtime evidence.
type State string

const (
	StateVerified      State = "VERIFIED"
	StateFailed        State = "FAILED_VERIFICATION"
	StateUnverifiable  State = "UNVERIFIABLE"
	StateNotApplicable State = "NOT_APPLICABLE"
)

type Receipt struct {
	SchemaVersion int         `json:"schema_version"`
	Subject       Subject     `json:"subject"`
	Artifact      Artifact    `json:"artifact"`
	Boot          Boot        `json:"boot"`
	Health        Health      `json:"health"`
	Event         Event       `json:"event"`
	SurfaceScan   SurfaceScan `json:"surface_scan"`
	Isolation     Isolation   `json:"isolation"`
	Cleanup       Cleanup     `json:"cleanup"`
}

type Subject struct {
	BeadID      string `json:"bead_id"`
	RunID       string `json:"run_id"`
	ProjectRoot string `json:"project_root"`
	GitHead     string `json:"git_head"`
	Host        string `json:"host"`
	CreatedAt   string `json:"created_at"`
}

type Artifact struct {
	Kind            string `json:"kind"`
	BuildPath       string `json:"build_path"`
	InstalledPath   string `json:"installed_path"`
	BuildDigest     string `json:"build_digest"`
	InstalledDigest string `json:"installed_digest"`
	RuntimeDigest   string `json:"runtime_digest"`
}

type Boot struct {
	StartedForProbe bool   `json:"started_for_probe"`
	ProcessID       int    `json:"process_id"`
	StartedAt       string `json:"started_at"`
	InstanceNonce   string `json:"instance_nonce"`
	ObservedNonce   string `json:"observed_nonce"`
	State           State  `json:"state"`
}

type Health struct {
	RequiredSubsystems []string          `json:"required_subsystems"`
	Observed           map[string]string `json:"observed"`
	FailureClasses     map[string]State  `json:"failure_classes"`
}

type Event struct {
	EventID         string      `json:"event_id"`
	ObservedEventID string      `json:"observed_event_id"`
	BeforeDigest    string      `json:"before_digest"`
	AfterDigest     string      `json:"after_digest"`
	Assertions      []Assertion `json:"assertions"`
}

type Assertion struct {
	Name     string `json:"name"`
	State    State  `json:"state"`
	Evidence string `json:"evidence"`
}

type SurfaceScan struct {
	Expected   []string `json:"expected"`
	Observed   []string `json:"observed"`
	Missing    []string `json:"missing"`
	Unexpected []string `json:"unexpected"`
}

type Isolation struct {
	Resources  []Resource `json:"resources"`
	Collisions []string   `json:"collisions"`
}

type Resource struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	Ownership   string `json:"ownership"`
}

// ResourceExpectation is trusted collector configuration sealed into run
// metadata. Fingerprints remain receipt observations because resource
// identifiers are generated at runtime and redacted before persistence.
type ResourceExpectation struct {
	Kind      string `json:"kind"`
	Ownership string `json:"ownership"`
}

// Expectations is the trusted proof scope derived from tracked collector
// configuration. A receipt may report observations but cannot reduce this set.
type Expectations struct {
	ExpectedBuildPath           string                `json:"expected_build_path"`
	ExpectedInstalledPath       string                `json:"expected_installed_path"`
	RequiredSubsystems          []string              `json:"required_subsystems"`
	NotApplicableFailureClasses map[string]bool       `json:"not_applicable_failure_classes"`
	RequiredAssertions          []string              `json:"required_assertions"`
	ExpectedSurfaces            []string              `json:"expected_surfaces"`
	RequiredResources           []ResourceExpectation `json:"required_resources"`
}

type Cleanup struct {
	OwnedResourcesRemaining []string `json:"owned_resources_remaining"`
}

// Summary is safe to persist in Beads. It intentionally excludes paths, PIDs,
// raw hostnames, resource identifiers, and probe evidence.
type Summary struct {
	SchemaVersion   int    `json:"schema_version"`
	ProofHash       string `json:"proof_hash"`
	RunID           string `json:"run_id"`
	GitHead         string `json:"git_head"`
	VerifiedAt      string `json:"verified_at"`
	HostFingerprint string `json:"host_fingerprint"`
}

type Result struct {
	Receipt   Receipt
	ProofHash string
	Summary   Summary
}

// Environment supplies bounded external reads and is injectable for tests.
type Environment struct {
	Now             func() time.Time
	Hostname        func() (string, error)
	GitHead         func(context.Context, string) (string, error)
	ReadRegularFile func(string, int64) ([]byte, error)
}

type VerifyOptions struct {
	ExpectedBeadID       string
	ExpectedRunID        string
	ExpectedProjectRoot  string
	ExpectedArtifactHash string
	RunCreatedAt         time.Time
	MaxAge               time.Duration
	MaxReceiptBytes      int64
	MaxArtifactBytes     int64
	Expectations         Expectations
	Environment          Environment
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var gitHeadPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// VerifyFile reads the receipt exactly once, verifies its registered hash, then
// decodes and validates those same bytes against current host and artifact state.
func VerifyFile(ctx context.Context, receiptPath string, opts VerifyOptions) (*Result, error) {
	opts = withDefaults(opts)
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	receiptBytes, err := opts.Environment.ReadRegularFile(receiptPath, opts.MaxReceiptBytes)
	if err != nil {
		return nil, fmt.Errorf("runtime evidence receipt: %w", err)
	}
	proofHash := hashBytes(receiptBytes)
	if proofHash != opts.ExpectedArtifactHash {
		return nil, fmt.Errorf("runtime evidence content hash mismatch: got %s, want %s", proofHash, opts.ExpectedArtifactHash)
	}

	receipt, err := decodeReceipt(receiptBytes)
	if err != nil {
		return nil, fmt.Errorf("decode runtime evidence: %w", err)
	}
	if err := validateReceipt(ctx, &receipt, opts); err != nil {
		return nil, err
	}

	now := opts.Environment.Now().UTC()
	return &Result{
		Receipt:   receipt,
		ProofHash: proofHash,
		Summary: Summary{
			SchemaVersion:   receipt.SchemaVersion,
			ProofHash:       proofHash,
			RunID:           receipt.Subject.RunID,
			GitHead:         receipt.Subject.GitHead,
			VerifiedAt:      now.Format(time.RFC3339Nano),
			HostFingerprint: hashBytes([]byte(receipt.Subject.Host)),
		},
	}, nil
}

func withDefaults(opts VerifyOptions) VerifyOptions {
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.MaxReceiptBytes <= 0 {
		opts.MaxReceiptBytes = DefaultMaxReceiptBytes
	}
	if opts.MaxArtifactBytes <= 0 {
		opts.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if opts.Environment.Now == nil {
		opts.Environment.Now = time.Now
	}
	if opts.Environment.Hostname == nil {
		opts.Environment.Hostname = os.Hostname
	}
	if opts.Environment.GitHead == nil {
		opts.Environment.GitHead = resolveGitHead
	}
	if opts.Environment.ReadRegularFile == nil {
		opts.Environment.ReadRegularFile = readRegularFile
	}
	return opts
}

func validateOptions(opts VerifyOptions) error {
	if opts.ExpectedBeadID == "" || opts.ExpectedRunID == "" || opts.ExpectedProjectRoot == "" {
		return errors.New("runtime evidence: expected bead, run, and project root are required")
	}
	if !filepath.IsAbs(opts.ExpectedProjectRoot) {
		return errors.New("runtime evidence: expected project root must be absolute")
	}
	if !digestPattern.MatchString(opts.ExpectedArtifactHash) {
		return errors.New("runtime evidence: expected artifact content hash is invalid")
	}
	if opts.RunCreatedAt.IsZero() {
		return errors.New("runtime evidence: run creation time is required")
	}
	if err := ValidateExpectations(opts.Expectations); err != nil {
		return fmt.Errorf("runtime evidence expectations: %w", err)
	}
	return nil
}

func decodeReceipt(b []byte) (Receipt, error) {
	var receipt Receipt
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return Receipt{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return Receipt{}, errors.New("multiple JSON values")
		}
		return Receipt{}, err
	}
	return receipt, nil
}

func validateReceipt(ctx context.Context, r *Receipt, opts VerifyOptions) error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("runtime evidence schema_version = %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if r.Subject.BeadID != opts.ExpectedBeadID {
		return fmt.Errorf("runtime evidence bead_id mismatch: got %q", r.Subject.BeadID)
	}
	if r.Subject.RunID != opts.ExpectedRunID {
		return fmt.Errorf("runtime evidence run_id mismatch: got %q", r.Subject.RunID)
	}
	if r.Subject.ProjectRoot != opts.ExpectedProjectRoot {
		return fmt.Errorf("runtime evidence project_root mismatch: got %q", r.Subject.ProjectRoot)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, r.Subject.CreatedAt)
	if err != nil {
		return fmt.Errorf("runtime evidence created_at: %w", err)
	}
	now := opts.Environment.Now()
	if now.Sub(createdAt) > opts.MaxAge {
		return fmt.Errorf("runtime evidence is stale: created_at %s", r.Subject.CreatedAt)
	}
	if createdAt.Before(opts.RunCreatedAt) {
		return fmt.Errorf("runtime evidence predates run: created_at %s", r.Subject.CreatedAt)
	}
	if createdAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("runtime evidence created_at is in the future: %s", r.Subject.CreatedAt)
	}

	host, err := opts.Environment.Hostname()
	if err != nil {
		return fmt.Errorf("runtime evidence host: %w", err)
	}
	if r.Subject.Host != host {
		return fmt.Errorf("runtime evidence host mismatch: got %q, want %q", r.Subject.Host, host)
	}
	gitHead, err := opts.Environment.GitHead(ctx, opts.ExpectedProjectRoot)
	if err != nil {
		return fmt.Errorf("runtime evidence git head: %w", err)
	}
	gitHead = strings.TrimSpace(gitHead)
	if !gitHeadPattern.MatchString(gitHead) || r.Subject.GitHead != gitHead {
		return fmt.Errorf("runtime evidence git_head mismatch: got %q, want %q", r.Subject.GitHead, gitHead)
	}

	if err := validateArtifact(r, opts); err != nil {
		return err
	}
	if err := validateBoot(r, opts.RunCreatedAt, createdAt); err != nil {
		return err
	}
	if err := validateHealth(r.Health, opts.Expectations); err != nil {
		return err
	}
	if err := validateEvent(r.Event, opts.Expectations); err != nil {
		return err
	}
	if err := validateSurfaces(r.SurfaceScan, opts.Expectations); err != nil {
		return err
	}
	if err := validateIsolation(r.Isolation, opts.Expectations); err != nil {
		return err
	}
	if r.Cleanup.OwnedResourcesRemaining == nil {
		return errors.New("runtime evidence cleanup missing field owned_resources_remaining")
	}
	if len(r.Cleanup.OwnedResourcesRemaining) != 0 {
		return fmt.Errorf("runtime evidence cleanup incomplete: %d owned resources remain", len(r.Cleanup.OwnedResourcesRemaining))
	}
	return nil
}

func validateArtifact(r *Receipt, opts VerifyOptions) error {
	a := r.Artifact
	if a.Kind != "file" {
		return fmt.Errorf("runtime evidence artifact kind %q is unsupported", a.Kind)
	}
	if !filepath.IsAbs(a.BuildPath) || !filepath.IsAbs(a.InstalledPath) {
		return errors.New("runtime evidence build_path and installed_path must be absolute")
	}
	if filepath.Clean(a.BuildPath) != filepath.Clean(opts.Expectations.ExpectedBuildPath) {
		return fmt.Errorf("runtime evidence build_path mismatch: got %q", a.BuildPath)
	}
	if filepath.Clean(a.InstalledPath) != filepath.Clean(opts.Expectations.ExpectedInstalledPath) {
		return fmt.Errorf("runtime evidence installed_path mismatch: got %q", a.InstalledPath)
	}
	if filepath.Clean(a.BuildPath) == filepath.Clean(a.InstalledPath) {
		return errors.New("runtime evidence build_path and installed_path must be distinct")
	}
	buildBytes, err := opts.Environment.ReadRegularFile(a.BuildPath, opts.MaxArtifactBytes)
	if err != nil {
		return fmt.Errorf("runtime evidence build artifact: %w", err)
	}
	installedBytes, err := opts.Environment.ReadRegularFile(a.InstalledPath, opts.MaxArtifactBytes)
	if err != nil {
		return fmt.Errorf("runtime evidence installed artifact: %w", err)
	}
	buildDigest := hashBytes(buildBytes)
	installedDigest := hashBytes(installedBytes)
	if !digestPattern.MatchString(a.BuildDigest) || a.BuildDigest != buildDigest {
		return fmt.Errorf("runtime evidence build_digest mismatch: got %q, want %q", a.BuildDigest, buildDigest)
	}
	if !digestPattern.MatchString(a.InstalledDigest) || a.InstalledDigest != installedDigest {
		return fmt.Errorf("runtime evidence installed_digest mismatch: got %q, want %q", a.InstalledDigest, installedDigest)
	}
	if !digestPattern.MatchString(a.RuntimeDigest) || a.RuntimeDigest != installedDigest {
		return fmt.Errorf("runtime evidence runtime_digest mismatch: got %q, want %q", a.RuntimeDigest, installedDigest)
	}
	if buildDigest != installedDigest {
		return errors.New("runtime evidence build and installed artifact digests differ")
	}
	return nil
}

func validateBoot(r *Receipt, runCreatedAt, receiptCreatedAt time.Time) error {
	b := r.Boot
	if !b.StartedForProbe {
		return errors.New("runtime evidence boot.started_for_probe must be true")
	}
	if b.ProcessID <= 0 {
		return errors.New("runtime evidence boot.process_id must be positive")
	}
	startedAt, err := time.Parse(time.RFC3339Nano, b.StartedAt)
	if err != nil {
		return fmt.Errorf("runtime evidence boot.started_at: %w", err)
	}
	if startedAt.Before(runCreatedAt) || startedAt.After(receiptCreatedAt) {
		return errors.New("runtime evidence boot.started_at is outside run/receipt chronology")
	}
	if b.InstanceNonce == "" || b.ObservedNonce != b.InstanceNonce {
		return errors.New("runtime evidence boot nonce mismatch")
	}
	if b.State != StateVerified {
		return fmt.Errorf("runtime evidence boot.state = %q, want VERIFIED", b.State)
	}
	return nil
}

// ValidateExpectations verifies the trusted proof scope before it is sealed in
// run metadata or used to validate a receipt.
func ValidateExpectations(e Expectations) error {
	if !filepath.IsAbs(e.ExpectedBuildPath) || !filepath.IsAbs(e.ExpectedInstalledPath) {
		return errors.New("expected build and installed paths must be absolute")
	}
	if filepath.Clean(e.ExpectedBuildPath) == filepath.Clean(e.ExpectedInstalledPath) {
		return errors.New("expected build and installed paths must be distinct")
	}
	if len(e.RequiredSubsystems) == 0 || hasBlankOrDuplicate(e.RequiredSubsystems) {
		return errors.New("required subsystems must be non-empty and unique")
	}
	if len(e.RequiredAssertions) == 0 || hasBlankOrDuplicate(e.RequiredAssertions) {
		return errors.New("required assertions must be non-empty and unique")
	}
	if len(e.ExpectedSurfaces) == 0 || hasBlankOrDuplicate(e.ExpectedSurfaces) {
		return errors.New("expected surfaces must be non-empty and unique")
	}
	allowedClasses := map[string]bool{
		"dependency_injection": true,
		"connection":           true,
		"projection_catchup":   true,
	}
	for class, allowed := range e.NotApplicableFailureClasses {
		if !allowed || !allowedClasses[class] {
			return fmt.Errorf("invalid NOT_APPLICABLE failure class %q", class)
		}
	}
	if len(e.RequiredResources) == 0 {
		return errors.New("required resources must be non-empty")
	}
	for _, resource := range e.RequiredResources {
		if resource.Kind != "port" && resource.Kind != "path" {
			return fmt.Errorf("required resource kind %q is unverifiable", resource.Kind)
		}
		if resource.Ownership != "ephemeral" && resource.Ownership != "exclusive" {
			return fmt.Errorf("required resource ownership %q is not isolated", resource.Ownership)
		}
	}
	return nil
}

func validateHealth(h Health, expected Expectations) error {
	if len(h.RequiredSubsystems) == 0 {
		return errors.New("runtime evidence requires at least one subsystem")
	}
	if hasBlankOrDuplicate(h.RequiredSubsystems) {
		return errors.New("runtime evidence required subsystem names must be unique and non-empty")
	}
	if !sameStrings(h.RequiredSubsystems, expected.RequiredSubsystems) {
		return errors.New("runtime evidence required subsystems do not match trusted expectations")
	}
	for _, name := range h.RequiredSubsystems {
		if h.Observed[name] != "healthy" {
			return fmt.Errorf("runtime evidence subsystem %q is not healthy", name)
		}
	}
	requiredClasses := []string{"startup", "dependency_injection", "connection", "projection_catchup"}
	if len(h.FailureClasses) != len(requiredClasses) {
		return errors.New("runtime evidence must report all four failure classes")
	}
	for _, class := range requiredClasses {
		state, ok := h.FailureClasses[class]
		if !ok {
			return fmt.Errorf("runtime evidence failure class %q is missing", class)
		}
		if class == "startup" && state != StateVerified {
			return fmt.Errorf("runtime evidence startup = %q, want VERIFIED", state)
		}
		if state == StateNotApplicable && !expected.NotApplicableFailureClasses[class] {
			return fmt.Errorf("runtime evidence failure class %s is NOT_APPLICABLE without authorization", class)
		}
		if state != StateVerified && state != StateNotApplicable {
			return fmt.Errorf("runtime evidence failure class %s = %q", class, state)
		}
	}
	return nil
}

func validateEvent(e Event, expected Expectations) error {
	if e.EventID == "" || e.ObservedEventID != e.EventID {
		return errors.New("runtime evidence event correlation mismatch")
	}
	if !digestPattern.MatchString(e.BeforeDigest) || !digestPattern.MatchString(e.AfterDigest) {
		return errors.New("runtime evidence event state digests are invalid")
	}
	if e.BeforeDigest == e.AfterDigest {
		return errors.New("runtime evidence event produced no state delta")
	}
	if len(e.Assertions) == 0 {
		return errors.New("runtime evidence requires at least one assertion")
	}
	names := make([]string, 0, len(e.Assertions))
	for _, assertion := range e.Assertions {
		if assertion.Name == "" || assertion.Evidence == "" || assertion.State != StateVerified {
			return fmt.Errorf("runtime evidence assertion %q is not VERIFIED with evidence", assertion.Name)
		}
		names = append(names, assertion.Name)
	}
	if hasBlankOrDuplicate(names) {
		return errors.New("runtime evidence assertion names must be unique and non-empty")
	}
	if !sameStrings(names, expected.RequiredAssertions) {
		return errors.New("runtime evidence required assertions do not match trusted expectations")
	}
	return nil
}

func validateSurfaces(s SurfaceScan, expected Expectations) error {
	if s.Missing == nil || s.Unexpected == nil {
		return errors.New("runtime evidence surface missing field: missing and unexpected must be present")
	}
	if len(s.Expected) == 0 || hasBlankOrDuplicate(s.Expected) || hasBlankOrDuplicate(s.Observed) {
		return errors.New("runtime evidence surface names must be non-empty and unique")
	}
	if !sameStrings(s.Expected, expected.ExpectedSurfaces) {
		return errors.New("runtime evidence expected surfaces do not match trusted expectations")
	}
	if len(s.Missing) != 0 || len(s.Unexpected) != 0 || !sameStrings(s.Expected, s.Observed) {
		return errors.New("runtime evidence surface scan mismatch")
	}
	return nil
}

func validateIsolation(i Isolation, expected Expectations) error {
	if len(i.Resources) == 0 {
		return errors.New("runtime evidence requires at least one isolated resource")
	}
	if i.Collisions == nil {
		return errors.New("runtime evidence isolation missing field collisions")
	}
	if len(i.Collisions) != 0 {
		return fmt.Errorf("runtime evidence isolation collision: %d reported", len(i.Collisions))
	}
	seen := make(map[string]struct{}, len(i.Resources))
	observedScope := make([]string, 0, len(i.Resources))
	for _, resource := range i.Resources {
		if resource.Kind != "port" && resource.Kind != "path" {
			return fmt.Errorf("runtime evidence resource kind %q is unverifiable", resource.Kind)
		}
		if resource.Ownership != "ephemeral" && resource.Ownership != "exclusive" {
			return fmt.Errorf("runtime evidence resource ownership %q is not isolated", resource.Ownership)
		}
		if !digestPattern.MatchString(resource.Fingerprint) {
			return errors.New("runtime evidence resource fingerprint is invalid")
		}
		if _, ok := seen[resource.Fingerprint]; ok {
			return errors.New("runtime evidence resource fingerprints must be unique")
		}
		seen[resource.Fingerprint] = struct{}{}
		observedScope = append(observedScope, resource.Kind+"\x00"+resource.Ownership)
	}
	expectedScope := make([]string, 0, len(expected.RequiredResources))
	for _, resource := range expected.RequiredResources {
		expectedScope = append(expectedScope, resource.Kind+"\x00"+resource.Ownership)
	}
	if !sameStrings(observedScope, expectedScope) {
		return errors.New("runtime evidence required resources do not match trusted expectations")
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for idx := range aa {
		if aa[idx] != bb[idx] {
			return false
		}
	}
	return true
}

func hasBlankOrDuplicate(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return true
		}
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func hashBytes(b []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	return readRegularFileWithHook(path, limit, nil)
}

func readRegularFileWithHook(path string, limit int64, beforeOpen func()) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%q is not an absolute path", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%q is too large (%d > %d bytes)", path, info.Size(), limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%q could not be opened", path)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%q changed between inspection and open", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%q is too large (%d > %d bytes)", path, len(b), limit)
	}
	return b, nil
}

func resolveGitHead(ctx context.Context, root string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitResolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
