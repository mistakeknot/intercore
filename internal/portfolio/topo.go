package portfolio

import "fmt"

// TopologicalSort returns projects in dependency-respecting order (upstreams first)
// using Kahn's algorithm with a min-heap priority queue. The output is deterministic:
// among nodes whose dependencies are all satisfied, the lexicographically smallest
// is always emitted first.
func TopologicalSort(deps []Dep) ([]string, error) {
	if len(deps) == 0 {
		return nil, nil
	}

	// Phase 1: Collect unique node names and assign indices.
	nameIndex := make(map[string]int, len(deps))
	var names []string
	nodeID := func(s string) int {
		if id, ok := nameIndex[s]; ok {
			return id
		}
		id := len(names)
		nameIndex[s] = id
		names = append(names, s)
		return id
	}
	for _, d := range deps {
		nodeID(d.UpstreamProject)
		nodeID(d.DownstreamProject)
	}
	n := len(names)

	// Phase 2: Build adjacency and in-degree arrays (zero-alloc per edge).
	inDegree := make([]int, n)
	// Flatten adjacency into a single slice pair (offsets + targets).
	// First pass: count outgoing edges.
	outCount := make([]int, n)
	for _, d := range deps {
		outCount[nameIndex[d.UpstreamProject]]++
	}
	// Compute prefix offsets.
	offsets := make([]int, n+1)
	for i := 0; i < n; i++ {
		offsets[i+1] = offsets[i] + outCount[i]
	}
	// Second pass: fill target array.
	targets := make([]int, len(deps))
	pos := make([]int, n) // write cursor per node
	copy(pos, offsets[:n])
	for _, d := range deps {
		u := nameIndex[d.UpstreamProject]
		v := nameIndex[d.DownstreamProject]
		targets[pos[u]] = v
		pos[u]++
		inDegree[v]++
	}

	// Phase 3: Kahn's with inline string min-heap for determinism.
	h := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if inDegree[i] == 0 {
			h = append(h, names[i])
		}
	}
	heapInit(h)

	order := make([]string, 0, n)
	for len(h) > 0 {
		var node string
		node, h = heapPop(h)
		order = append(order, node)
		uid := nameIndex[node]
		for j := offsets[uid]; j < offsets[uid+1]; j++ {
			v := targets[j]
			inDegree[v]--
			if inDegree[v] == 0 {
				h = heapPush(h, names[v])
			}
		}
	}

	if len(order) != n {
		return nil, fmt.Errorf("topo sort: cycle detected (%d nodes, %d sorted)", n, len(order))
	}
	return order, nil
}

// Inline min-heap operations on []string to avoid container/heap interface boxing.

func heapInit(h []string) {
	n := len(h)
	for i := n/2 - 1; i >= 0; i-- {
		heapDown(h, i, n)
	}
}

func heapPush(h []string, s string) []string {
	h = append(h, s)
	heapUp(h, len(h)-1)
	return h
}

func heapPop(h []string) (string, []string) {
	n := len(h) - 1
	h[0], h[n] = h[n], h[0]
	heapDown(h, 0, n)
	return h[n], h[:n]
}

func heapUp(h []string, j int) {
	for {
		i := (j - 1) / 2
		if i == j || h[j] >= h[i] {
			break
		}
		h[i], h[j] = h[j], h[i]
		j = i
	}
}

func heapDown(h []string, i, n int) {
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		j := left
		if right := left + 1; right < n && h[right] < h[left] {
			j = right
		}
		if h[i] <= h[j] {
			break
		}
		h[i], h[j] = h[j], h[i]
		i = j
	}
}
