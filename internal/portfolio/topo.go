package portfolio

import "fmt"

// TopologicalSort returns projects in dependency-respecting order (upstreams first)
// using Kahn's algorithm. Returns an error if a cycle is detected.
func TopologicalSort(deps []Dep) ([]string, error) {
	// Build adjacency list and in-degree map
	inDegree := make(map[string]int)
	downstream := make(map[string][]string)

	// Collect all nodes
	for _, d := range deps {
		if _, ok := inDegree[d.UpstreamProject]; !ok {
			inDegree[d.UpstreamProject] = 0
		}
		if _, ok := inDegree[d.DownstreamProject]; !ok {
			inDegree[d.DownstreamProject] = 0
		}
		downstream[d.UpstreamProject] = append(downstream[d.UpstreamProject], d.DownstreamProject)
		inDegree[d.DownstreamProject]++
	}

	// Seed queue with zero-in-degree nodes
	var queue []string
	for node, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, node)
		}
	}

	var order []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		order = append(order, node)
		for _, next := range downstream[node] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(order) != len(inDegree) {
		return nil, fmt.Errorf("topo sort: cycle detected (%d nodes, %d sorted)", len(inDegree), len(order))
	}
	return order, nil
}
