package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/workerssh"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// SetCommands attaches the shared agent acknowledgement broker before serving plans.
func (s *Service) SetCommands(broker *commands.Broker) { s.commands = broker }

// workerSSHError distinguishes unavailable preflight from invalid launch settings.
type workerSSHError struct{ err error }

func (e *workerSSHError) Error() string { return e.err.Error() }
func (e *workerSSHError) Unwrap() error { return e.err }

func workerSSHAddresses(inv *inventory.Inventory) []string {
	var addresses []string
	add := func(address string) {
		address = strings.TrimSuffix(strings.TrimPrefix(strings.Split(address, "/")[0], "["), "]")
		if address == "" || slices.Contains(addresses, address) {
			return
		}
		if ip := net.ParseIP(address); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
			return
		}
		addresses = append(addresses, address)
	}
	add(firstNonLoopback(inv))
	add(inv.AdvertiseAddress)
	for _, iface := range inv.Interfaces {
		for _, address := range iface.Addresses {
			add(address)
		}
	}
	return addresses
}

func sameSSHAddress(left, right string) bool {
	leftIP, rightIP := net.ParseIP(left), net.ParseIP(right)
	return strings.EqualFold(left, right) || (leftIP != nil && rightIP != nil && leftIP.Equal(rightIP))
}

func (s *Service) workerSSHInventory(ctx context.Context, nodeID string) (*inventory.Inventory, error) {
	node, err := s.q.GetNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("selected node %q is unavailable: %w", nodeID, err)
	}
	if !node.Inventory.Valid || node.Inventory.String == "" || node.Inventory.String == "null" {
		return nil, fmt.Errorf("selected node %q has no agent inventory", nodeID)
	}
	inv, err := inventory.Parse(node.Inventory.String)
	if err != nil {
		return nil, fmt.Errorf("selected node %q has invalid agent inventory: %w", nodeID, err)
	}
	return inv, nil
}

func selectedWorkerSSHNode(placements []PlacementOverride, rank int32) (string, error) {
	nodeID := ""
	for _, placement := range placements {
		if placement.Rank != rank {
			continue
		}
		if nodeID != "" {
			return "", fmt.Errorf("worker SSH requires exactly one selected node for rank %d", rank)
		}
		nodeID = placement.NodeID
	}
	if nodeID == "" {
		return "", fmt.Errorf("worker SSH requires a selected node for rank %d", rank)
	}
	return nodeID, nil
}

func (s *Service) defaultWorkerSSH(ctx context.Context, workload *recipe.Workload, placements []PlacementOverride, workerID string, inv *inventory.Inventory, resolved map[string]string) (string, error) {
	rank := int32(0)
	if workload.Upstream.CoordinatorRank != nil {
		rank = int32(*workload.Upstream.CoordinatorRank)
	}
	headID, err := selectedWorkerSSHNode(placements, rank)
	if err != nil {
		return "", &workerSSHError{err}
	}
	key := headID + "\x00" + workerID
	if target, ok := resolved[key]; ok {
		return target, nil
	}
	if inv.AgentUsername == "" {
		return "", fmt.Errorf("agent inventory does not report the process account username")
	}
	result, err := s.requestWorkerSSH(ctx, headID, workerID, workerssh.Request{Addresses: workerSSHAddresses(inv), Username: inv.AgentUsername, ResolveOnly: true})
	if err != nil {
		return "", &workerSSHError{err}
	}
	resolved[key] = result.Target
	return result.Target, nil
}

func (s *Service) requestWorkerSSH(ctx context.Context, headID, workerID string, request workerssh.Request) (workerssh.Result, error) {
	var result workerssh.Result
	if s.commands == nil {
		return result, fmt.Errorf("worker SSH preflight is unavailable: command broker is not configured")
	}
	if s.nodes == nil || !s.nodes.Online(headID) || !s.nodes.Online(workerID) {
		return result, fmt.Errorf("worker SSH preflight requires online head %q and worker %q", headID, workerID)
	}
	head, err := s.workerSSHInventory(ctx, headID)
	if err != nil {
		return result, err
	}
	if !slices.Contains(head.ProtocolFeatures, workerssh.ProtocolFeature) {
		return result, fmt.Errorf("head node %q needs an updated agent supporting %s before worker SSH can be checked", headID, workerssh.ProtocolFeature)
	}
	// Container bridges commonly repeat across nodes; an alias to the head's
	// own address must never become the selected worker's inferred destination.
	headAddresses := workerSSHAddresses(head)
	onHead := func(address string) bool {
		return slices.ContainsFunc(headAddresses, func(local string) bool { return sameSSHAddress(address, local) })
	}
	if slices.ContainsFunc(request.Addresses, onHead) {
		request.Addresses = slices.DeleteFunc(slices.Clone(request.Addresses), onHead)
	}
	if len(request.Addresses) == 0 {
		return result, fmt.Errorf("selected worker %q has no inventory addresses for SSH verification", workerID)
	}
	commandID, err := id.New()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	responses, release := s.commands.Wait(commandID)
	defer release()
	if !s.nodes.Send(headID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_WorkerSshCommand{WorkerSshCommand: &agentv1.WorkerSSHCommand{
		CommandId: commandID, Addresses: request.Addresses, Username: request.Username, Target: request.Target, ResolveOnly: request.ResolveOnly,
	}}}) {
		return result, fmt.Errorf("head node %q disconnected before worker SSH preflight", headID)
	}
	select {
	case <-ctx.Done():
		return result, fmt.Errorf("worker SSH preflight on head %q: %w", headID, ctx.Err())
	case response := <-responses:
		if response == nil || !response.Ok {
			message := "missing acknowledgement"
			if response != nil && response.Error != "" {
				message = response.Error
			}
			return result, fmt.Errorf("worker SSH from head %q to selected worker %q failed: %s", headID, workerID, message)
		}
		if len(response.OutputJson) > 16384 || json.Unmarshal(response.OutputJson, &result) != nil || result.Target == "" || result.Hostname == "" || result.Username == "" {
			return result, fmt.Errorf("head %q returned an invalid worker SSH acknowledgement", headID)
		}
		if request.Target != "" && result.Target != request.Target {
			return result, fmt.Errorf("head %q checked a different SSH target than the rendered source target %q", headID, request.Target)
		}
		if request.ResolveOnly && result.Username != request.Username {
			return result, fmt.Errorf("head %q resolved a different SSH account than selected worker %q", headID, workerID)
		}
		matched := false
		for _, address := range request.Addresses {
			if sameSSHAddress(result.Hostname, address) {
				matched = true
				break
			}
		}
		if !matched {
			return result, fmt.Errorf("SSH target %q resolves to %q, not selected worker %q", result.Target, result.Hostname, workerID)
		}
		return result, nil
	}
}

// Only authored worker SSH bindings imply a remote shell. Fabric IP remains a
// transport address; it is used verbatim only when the source combines IP+USER.
func (s *Service) previewWorkerSSH(ctx context.Context, plan *Plan) {
	placements := make([]PlacementOverride, 0, len(plan.Placements))
	for _, placement := range plan.Placements {
		placements = append(placements, PlacementOverride{NodeID: placement.NodeID, Rank: placement.Rank})
	}
	checked := map[string]bool{}
	for _, upstream := range plan.Upstream {
		if upstream.Execution.CoordinatorRank != nil && int(upstream.Rank) != *upstream.Execution.CoordinatorRank {
			continue
		}
		type binding struct{ key, target string }
		targets := map[int32][]binding{}
		ips, users := map[int32]string{}, map[int32]string{}
		for _, key := range slices.Sorted(maps.Keys(upstream.Environment)) {
			lookup := key
			if strings.HasSuffix(key, "_IP") {
				lookup = strings.TrimSuffix(key, "_IP") + "_SSH"
			}
			rank, fact, ok := workerSettingBinding(lookup)
			if !ok {
				continue
			}
			value := upstream.Environment[key]
			if strings.HasSuffix(key, "_IP") {
				ips[rank] = value
			} else if fact == "SSH" || fact == "HOST" {
				targets[rank] = append(targets[rank], binding{key, value})
			} else if fact == "USER" {
				users[rank] = value
			}
		}
		for rank, address := range ips {
			if len(targets[rank]) == 0 {
				if username, ok := users[rank]; ok {
					target := address
					if username != "" {
						target = username + "@" + address
					}
					targets[rank] = []binding{{"WORKER_IP + WORKER_USER", target}}
				}
			}
		}
		for _, rank := range slices.Sorted(maps.Keys(targets)) {
			for _, target := range targets[rank] {
				workerID, err := selectedWorkerSSHNode(placements, rank)
				if err == nil {
					key := upstream.NodeID + "\x00" + workerID + "\x00" + target.target
					if checked[key] {
						continue
					}
					checked[key] = true
					var inv *inventory.Inventory
					inv, err = s.workerSSHInventory(ctx, workerID)
					if err == nil {
						if target.target == "" {
							err = fmt.Errorf("rendered SSH target is empty")
						} else {
							_, err = s.requestWorkerSSH(ctx, upstream.NodeID, workerID, workerssh.Request{Addresses: workerSSHAddresses(inv), Username: inv.AgentUsername, Target: target.target})
						}
					}
				}
				if err != nil {
					plan.Diagnostics = append(plan.Diagnostics, diag.Error("upstream.worker_ssh_unavailable", fmt.Sprintf("%s %q (head %s, worker rank %d): %v", target.key, target.target, upstream.NodeID, rank, err)))
				}
			}
		}
	}
}
