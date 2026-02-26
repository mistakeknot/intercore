# Intercore Compatibility Contract

> **Version:** 1.0 (applies from Intercore v1.0)
>
> This document defines what is stable, what may change, and how breaking changes are handled. It is the reference for external consumers building on Intercore.

## CLI Surface (stable from v1)

- Command names, required flags, and exit codes are backward-compatible across minor versions.
- New commands and optional flags may be added in minor versions.
- Existing commands are deprecated (with warnings) for one minor version before removal.

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Gate failure / expected rejection |
| 2 | Usage error |
| 3+ | Internal error |

## Event Schema (stable from v1)

- Event type strings (e.g., `phase.advanced`, `dispatch.completed`) are stable across minor versions.
- New event types may be added. Existing event types are never renamed or removed without a major version bump.
- Event payload fields may gain new optional fields. Existing fields are never removed or type-changed without a major version bump.
- Consumers should ignore unknown event types and unknown fields (forward-compatible parsing).
- Event envelope `envelope` is additive and optional. New optional envelope fields may be introduced in minor versions.

Current optional envelope fields (v2):
- `policy_version`
- `caller_identity`
- `capability_scope`
- `trace_id`, `span_id`, `parent_span_id`
- `input_artifact_refs`, `output_artifact_refs`
- `requested_sandbox`, `effective_sandbox`

## Database Schema (migration-safe)

- Schema changes use `PRAGMA user_version` for versioning and `ic init` for auto-migration.
- Migrations are forward-only (no downgrades). `ic init` creates a timestamped backup before any migration.
- New columns use `ALTER TABLE ADD COLUMN` with defaults (never `NOT NULL` without a default on existing tables).

## Security Invariants

- The database file is created with `0600` permissions (owner read/write only).
- The `.clavain/` directory is created with `0700` permissions.
- The kernel never stores secret values in the database. Payloads are validated against common secret patterns (API keys, tokens, JWTs, PEM keys). Store variable names for provenance, not secret values.

## What Is NOT Stable

These are internal implementation details that may change without notice:

- **Internal Go API** — there is no library API in v1. The CLI is the contract.
- **Database file layout and internal table structures** — callers should use `ic` commands, not raw SQL.
- **Debug output format** — stderr messages may change without notice.
- **Lock file format** — filesystem locks are an internal mechanism.

## Deprecation Policy

1. A command or flag is marked deprecated with a warning message for one minor version.
2. The deprecated item continues to work during the deprecation period.
3. The item is removed in the next minor version after the deprecation period.
4. Breaking changes to event schemas or exit codes require a major version bump.

## Versioning

Intercore follows [Semantic Versioning](https://semver.org/):

- **Patch** (0.x.Y): Bug fixes, no behavior changes.
- **Minor** (0.X.0): New features, new commands, new event types. Backward-compatible.
- **Major** (X.0.0): Breaking changes to CLI, event schemas, or exit codes.

## Reporting Issues

If you believe a release violates this contract, please file an issue. Unintentional breaking changes in minor releases are treated as bugs.
