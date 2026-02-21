package portfolio

import (
	"strings"
	"testing"
)

func TestTopologicalSort_Linear(t *testing.T) {
	deps := []Dep{
		{UpstreamProject: "A", DownstreamProject: "B"},
		{UpstreamProject: "B", DownstreamProject: "C"},
	}
	order, err := TopologicalSort(deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 {
		t.Fatalf("order = %v, want 3 elements", order)
	}
	idx := make(map[string]int)
	for i, p := range order {
		idx[p] = i
	}
	if idx["A"] > idx["B"] {
		t.Errorf("A should come before B: %v", order)
	}
	if idx["B"] > idx["C"] {
		t.Errorf("B should come before C: %v", order)
	}
}

func TestTopologicalSort_Diamond(t *testing.T) {
	// A → B, A → C, B → D, C → D
	deps := []Dep{
		{UpstreamProject: "A", DownstreamProject: "B"},
		{UpstreamProject: "A", DownstreamProject: "C"},
		{UpstreamProject: "B", DownstreamProject: "D"},
		{UpstreamProject: "C", DownstreamProject: "D"},
	}
	order, err := TopologicalSort(deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 {
		t.Fatalf("order = %v, want 4 elements", order)
	}
	idx := make(map[string]int)
	for i, p := range order {
		idx[p] = i
	}
	if idx["A"] > idx["B"] || idx["A"] > idx["C"] {
		t.Errorf("A should come first: %v", order)
	}
	if idx["B"] > idx["D"] || idx["C"] > idx["D"] {
		t.Errorf("D should come last: %v", order)
	}
}

func TestTopologicalSort_Forest(t *testing.T) {
	// Two independent chains: A→B and C→D
	deps := []Dep{
		{UpstreamProject: "A", DownstreamProject: "B"},
		{UpstreamProject: "C", DownstreamProject: "D"},
	}
	order, err := TopologicalSort(deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 {
		t.Fatalf("order = %v, want 4 elements", order)
	}
	idx := make(map[string]int)
	for i, p := range order {
		idx[p] = i
	}
	if idx["A"] > idx["B"] {
		t.Errorf("A should come before B: %v", order)
	}
	if idx["C"] > idx["D"] {
		t.Errorf("C should come before D: %v", order)
	}
}

func TestTopologicalSort_Empty(t *testing.T) {
	order, err := TopologicalSort(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 {
		t.Errorf("order = %v, want empty", order)
	}
}

func TestTopologicalSort_Cycle(t *testing.T) {
	deps := []Dep{
		{UpstreamProject: "A", DownstreamProject: "B"},
		{UpstreamProject: "B", DownstreamProject: "C"},
		{UpstreamProject: "C", DownstreamProject: "A"},
	}
	_, err := TopologicalSort(deps)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, expected to contain 'cycle'", err.Error())
	}
}
