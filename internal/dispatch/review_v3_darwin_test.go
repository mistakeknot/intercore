//go:build darwin

package dispatch

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReviewV3DarwinBirthUsesBootSessionUUID(t *testing.T) {
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if boot == "" || !strings.HasPrefix(identity.Birth, "darwin:"+boot+":") {
		t.Fatalf("birth does not use boot-session identity: %q", identity.Birth)
	}
}
