package runs

import "strings"

// ResourcesConflict preserves ordinary resource sharing, but a source-owned
// lifecycle cannot share a node's managed compute/port leases: its authored
// commands, not the controller, select Docker devices and host resources.
func ResourcesConflict(left, right string) bool {
	if left == right {
		return true
	}
	return upstreamNodeConflict(left, right) || upstreamNodeConflict(right, left)
}

func upstreamNodeConflict(upstream, resource string) bool {
	node, ok := strings.CutPrefix(upstream, "upstream-node:")
	if !ok || node == "" {
		return false
	}
	kind, rest, ok := strings.Cut(resource, ":")
	if !ok {
		return false
	}
	switch kind {
	case "node", "upstream-node":
		return rest == node
	case "gpu", "port":
		resourceNode, _, _ := strings.Cut(rest, ":")
		return resourceNode == node
	default:
		return false
	}
}
