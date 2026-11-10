package runner

import (
	"errors"
	"testing"
)

const statLine = "4242 (a b) c) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"

func TestParseStatHandlesParensInCommand(t *testing.T) {
	state, start, err := ParseStat(statLine)
	if err != nil || state != 'S' || start != 987654 {
		t.Fatalf("got %c %d %v", state, start, err)
	}
	for _, bad := range []string{"", "1 (x)", "1 (x) S 1 2"} {
		if _, _, err := ParseStat(bad); err == nil {
			t.Errorf("ParseStat(%q) accepted", bad)
		}
	}
}

func TestHolderPresent(t *testing.T) {
	read := func(text string, err error) func(int) (string, error) {
		return func(int) (string, error) { return text, err }
	}
	h := Holder{PID: 4242, StartTime: 987654, BootID: "b1"}
	zombie := "4242 (x) Z 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	stopped := "4242 (x) T 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	reused := "4242 (x) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 111 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	cases := []struct {
		name string
		h    Holder
		boot string
		read func(int) (string, error)
		want bool
	}{
		{"alive", h, "b1", read(statLine, nil), true},
		{"zombie counts as present", h, "b1", read(zombie, nil), true},
		{"stopped counts as present", h, "b1", read(stopped, nil), true},
		{"pid reused", h, "b1", read(reused, nil), false},
		{"pid absent", h, "b1", read("", errors.New("no such process")), false},
		{"other boot", h, "b2", read(statLine, nil), false},
		{"no pid recorded", Holder{}, "b1", read(statLine, nil), false},
		{"unreadable stat is present", h, "b1", read("garbage", nil), true},
	}
	for _, c := range cases {
		if got := c.h.Present(c.boot, c.read); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
