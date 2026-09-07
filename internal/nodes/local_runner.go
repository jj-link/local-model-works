package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
)

var (
	ErrNoApprovedLocalRunner       = errors.New("no approved local runner is registered")
	ErrMultipleApprovedLocalRunner = errors.New("multiple approved local runners match this host")
)

type nodeLister interface {
	ListNodes(context.Context) ([]db.Node, error)
}

type runnerNodeInventory struct {
	Hostname string `json:"hostname"`
}

// ApprovedLocalRunnerNodeID resolves the approved node co-located with the
// controller. A unique online match wins; a unique offline match is returned
// so the caller can report deployment health separately.
func ApprovedLocalRunnerNodeID(ctx context.Context, query nodeLister, registry *Registry) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve local runner hostname: %w", err)
	}
	nodeRows, err := query.ListNodes(ctx)
	if err != nil {
		return "", fmt.Errorf("list nodes for local runner: %w", err)
	}
	return approvedLocalRunnerNodeID(hostname, nodeRows, registry)
}

func approvedLocalRunnerNodeID(hostname string, nodeRows []db.Node, registry *Registry) (string, error) {
	var matching, online []string
	for _, node := range nodeRows {
		if node.Status == "pending" || !node.Inventory.Valid {
			continue
		}
		var inventory runnerNodeInventory
		if err := json.Unmarshal([]byte(node.Inventory.String), &inventory); err != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(inventory.Hostname), strings.TrimSpace(hostname)) {
			continue
		}
		matching = append(matching, node.ID)
		if registry != nil && registry.Online(node.ID) {
			online = append(online, node.ID)
		}
	}

	switch len(online) {
	case 1:
		return online[0], nil
	case 0:
		switch len(matching) {
		case 0:
			return "", ErrNoApprovedLocalRunner
		case 1:
			return matching[0], nil
		}
	}
	return "", ErrMultipleApprovedLocalRunner
}
