package portfolio

import (
	"fmt"
	"testing"
)

func makeDeps(n int) []Dep {
	deps := make([]Dep, 0, n)
	for i := 0; i < n; i++ {
		deps = append(deps, Dep{
			UpstreamProject:   fmt.Sprintf("proj-%d", i),
			DownstreamProject: fmt.Sprintf("proj-%d", i+1),
		})
	}
	return deps
}

func makeDiamond(n int) []Dep {
	deps := make([]Dep, 0, n*2)
	for i := 0; i < n; i++ {
		hub := fmt.Sprintf("hub-%d", i)
		deps = append(deps,
			Dep{UpstreamProject: "root", DownstreamProject: hub},
			Dep{UpstreamProject: hub, DownstreamProject: "leaf"},
		)
	}
	return deps
}

func BenchmarkTopologicalSort10(b *testing.B) {
	deps := makeDeps(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = TopologicalSort(deps)
	}
}

func BenchmarkTopologicalSort50(b *testing.B) {
	deps := makeDeps(50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = TopologicalSort(deps)
	}
}

func BenchmarkTopologicalSortDiamond20(b *testing.B) {
	deps := makeDiamond(20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = TopologicalSort(deps)
	}
}
