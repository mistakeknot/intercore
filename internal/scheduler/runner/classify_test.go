package runner

import (
	"testing"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Unit test 9: one case per precedence row plus the overlaps.
func TestClassifyPrecedence(t *testing.T) {
	failedV := func(v, ev string) Evidence {
		return Evidence{DispatchPresent: true, Status: "failed", Verdict: v, EvidenceClass: ev}
	}
	cases := []struct {
		name string
		in   Evidence
		want string
	}{
		{"1 cancelling job beats completed", Evidence{Cancelling: true, DispatchPresent: true, Status: "completed"}, scheduler.ClassCancelled},
		{"1 dispatch cancelled", Evidence{DispatchPresent: true, Status: "cancelled"}, scheduler.ClassCancelled},
		{"2 completed", Evidence{DispatchPresent: true, Status: "completed"}, scheduler.ClassOK},
		{"3 spawn_failed", Evidence{DispatchPresent: true, Status: "failed", FailureClass: "spawn_failed"}, scheduler.ClassPrestart},
		{"3 spawn_failed beats missing verdict", Evidence{DispatchPresent: true, Status: "failed", FailureClass: "spawn_failed", EvidenceClass: "quota_exhausted"}, scheduler.ClassPrestart},
		{"4b dispatch-written indeterminate failure class beats a valid verdict", Evidence{DispatchPresent: true, Status: "failed", FailureClass: "worker_outcome_indeterminate", Verdict: "pass", EvidenceClass: "none"}, scheduler.ClassIndeterminate},
		{"4 indeterminate failure class", Evidence{DispatchPresent: true, Status: "failed", FailureClass: "indeterminate", Verdict: "fail", EvidenceClass: "none"}, scheduler.ClassIndeterminate},
		{"5 timeout", Evidence{DispatchPresent: true, Status: "timeout"}, scheduler.ClassTimeout},
		{"6 pruned", Evidence{DispatchPresent: false}, scheduler.ClassIndeterminate},
		{"6 never terminalized", Evidence{DispatchPresent: true, Status: "running"}, scheduler.ClassIndeterminate},
		{"7 no verdict", failedV("", "none"), scheduler.ClassIndeterminate},
		{"7 no verdict with capacity receipt stays indeterminate", failedV("", "quota_exhausted"), scheduler.ClassIndeterminate},
		{"7 malformed verdict", failedV("banana", "rate_limited"), scheduler.ClassIndeterminate},
		{"8 quota_exhausted", failedV("fail", "quota_exhausted"), scheduler.ClassCapacity},
		{"8 rate_limited", failedV("fail", "rate_limited"), scheduler.ClassCapacity},
		{"9 other known class", failedV("fail", "auth_error"), scheduler.ClassAgentFailed},
		{"10 evidence none is never agent_failed", failedV("fail", "none"), scheduler.ClassFailedUnclassified},
		{"10 warn verdict, no evidence", failedV("warn", "none"), scheduler.ClassFailedUnclassified},
		{"11 unknown status", Evidence{DispatchPresent: true, Status: "weird"}, scheduler.ClassIndeterminate},
		{"11 failed, evidence not yet written", failedV("fail", ""), scheduler.ClassUnknown},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestClassifyIsPure(t *testing.T) {
	in := Evidence{DispatchPresent: true, Status: "failed", Verdict: "fail", EvidenceClass: "quota_exhausted"}
	a, b := Classify(in), Classify(in)
	if a != b || a != scheduler.ClassCapacity {
		t.Fatalf("%s %s", a, b)
	}
}

func TestNormalizeEvidenceClass(t *testing.T) {
	for in, want := range map[string]string{
		"quota_exhausted": "quota_exhausted", "rate_limited": "rate_limited",
		"auth_error": "auth_error", "": "none", "none": "none",
		"Quota_Exhausted": "none", "../../etc": "none", "weird class": "none",
	} {
		if got := NormalizeEvidenceClass(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
