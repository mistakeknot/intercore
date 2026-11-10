package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Unit test 10.
func TestLiveDBGuard(t *testing.T) {
	home := t.TempDir()
	live := filepath.Join(home, ".clavain")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(live, "intercore.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	canon, _ := CanonicalDB(db)
	if err := CheckLiveDB(canon, home, false); !errors.Is(err, ErrRefused) {
		t.Fatalf("live path under $HOME accepted: %v", err)
	}
	if err := CheckLiveDB(canon, home, true); err != nil {
		t.Fatalf("override ignored: %v", err)
	}
	// Through a symlink: a DB in a scratch dir that points into the live dir.
	scratch := t.TempDir()
	link := filepath.Join(scratch, "intercore.db")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	c2, err := CanonicalDB(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLiveDB(c2, home, false); !errors.Is(err, ErrRefused) {
		t.Fatalf("symlink into the live directory accepted: %v", err)
	}
	// A scratch DB is accepted, and so is a sibling directory sharing a prefix.
	if err := CheckLiveDB(filepath.Join(scratch, "x.db"), home, false); err != nil {
		t.Fatalf("scratch refused: %v", err)
	}
	if err := CheckLiveDB(filepath.Join(home, ".clavain-other", "x.db"), home, false); err != nil {
		t.Fatalf("prefix sibling refused: %v", err)
	}
	// Any /home/*/.clavain/, whoever owns it.
	for _, p := range []string{"/home/someone/.clavain/intercore.db", "/home/a/.clavain/x/y.db"} {
		if err := CheckLiveDB(p, "/nowhere", false); !errors.Is(err, ErrRefused) {
			t.Errorf("%s accepted", p)
		}
	}
	for _, p := range []string{"/home/someone/.clavainx/a.db", "/home/.clavain/a.db", "/tmp/home/x/.clavain/a.db"} {
		if err := CheckLiveDB(p, "/nowhere", false); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
}

const mountinfoSample = `22 28 0:21 / /sys rw,nosuid shared:7 - sysfs sysfs rw
36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
99 28 8:1 /db.sqlite /srv/with\040space/db.sqlite rw - ext4 /dev/sda1 rw
`

func TestMountPointParse(t *testing.T) {
	mps, err := MountPoints(strings.NewReader(mountinfoSample))
	if err != nil || len(mps) != 3 || mps[2] != "/srv/with space/db.sqlite" {
		t.Fatalf("%v %v", mps, err)
	}
	for path, want := range map[string]bool{"/srv/with space/db.sqlite": true, "/mnt2": true, "/mnt2/x": false, "/srv": false} {
		got, _ := IsMountPoint(path, strings.NewReader(mountinfoSample))
		if got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

func TestCheckPathRefusesHardlinkAndBindMount(t *testing.T) {
	p := PathInfo{Canonical: "/srv/a.db", Nlink: 1}
	if err := CheckPath(p, strings.NewReader(mountinfoSample)); err != nil {
		t.Fatal(err)
	}
	p.Nlink = 2
	if err := CheckPath(p, strings.NewReader(mountinfoSample)); !errors.Is(err, ErrRefused) {
		t.Fatalf("hardlink accepted: %v", err)
	}
	p = PathInfo{Canonical: "/mnt2", Nlink: 1}
	if err := CheckPath(p, strings.NewReader(mountinfoSample)); !errors.Is(err, ErrRefused) {
		t.Fatalf("bind mount accepted: %v", err)
	}
}

func TestUnsafeFS(t *testing.T) {
	for _, m := range []int64{0x6969, 0xFF534D42, 0xFE534D42, 0x65735546} {
		if UnsafeFS(m) == "" {
			t.Errorf("%#x accepted", m)
		}
	}
	if UnsafeFS(0xEF53) != "" || UnsafeFS(0x01021994) != "" {
		t.Error("ext4 or tmpfs refused")
	}
}
