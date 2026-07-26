package publish

import (
	"os"
	"path/filepath"
	"strings"
)

// PublishRole describes whether this machine is allowed to be a source of
// plugin versions.
//
// Why this exists (mk-ldnb): publishes run on the signer machine (zklw). Other
// machines hold checkouts that are never version-bumped, so `ic publish doctor`
// reported 21 plugins as "plugin.json=0.2.2 marketplace=0.2.3" errors on
// Clavain. Those are not faults -- they are the expected steady state of a
// machine that does not publish.
//
// Worse, `--fix` responded to that drift by writing the LOCAL version into the
// marketplace, which on a non-publishing machine means downgrading the
// marketplace to a stale version. Twenty-one of them, in one command.
type PublishRole string

const (
	// RoleSigner publishes. Local plugin.json is authoritative.
	RoleSigner PublishRole = "signer"
	// RoleVerifier does not publish. The marketplace is authoritative and a
	// local checkout trailing it is normal.
	RoleVerifier PublishRole = "verifier"
)

// RoleFilePath is where the role is recorded when not set via the environment.
func RoleFilePath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "intercore", "publish-role")
	}
	return ""
}

// CurrentPublishRole resolves this machine's role.
//
// Order: IC_PUBLISH_ROLE, then ~/.config/intercore/publish-role, then the
// default. The default is RoleSigner so that machines which never opt in behave
// exactly as before -- an unconfigured machine is not silently downgraded into
// a mode where it stops reporting real drift.
func CurrentPublishRole() PublishRole {
	if v := strings.TrimSpace(os.Getenv("IC_PUBLISH_ROLE")); v != "" {
		return normaliseRole(v)
	}
	if p := RoleFilePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return normaliseRole(v)
			}
		}
	}
	return RoleSigner
}

func normaliseRole(v string) PublishRole {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(RoleVerifier):
		return RoleVerifier
	default:
		return RoleSigner
	}
}
