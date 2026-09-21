//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureUpstreamProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

func upstreamProcessIdentity(pid int) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("upstream.process_identity_invalid")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", fmt.Errorf("upstream.process_stat_invalid")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return "", fmt.Errorf("upstream.process_exited")
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19], nil
}

func upstreamProcessAlive(pid int, identity string) bool {
	if identity == "" {
		return false
	}
	actual, err := upstreamProcessIdentity(pid)
	return err == nil && actual == identity
}

func terminateUpstreamProcess(pid int, identity string, force bool) error {
	if !upstreamProcessAlive(pid, identity) {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if force {
		deadline := time.Now().Add(5 * time.Second)
		for upstreamProcessAlive(pid, identity) {
			if time.Now().After(deadline) {
				return fmt.Errorf("upstream.process_group_did_not_exit")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return nil
}

// Keep the session leader alive while authored background group members exist.
// Its boot/start-time identity therefore remains usable after agent reconnect.
// A failed/cancelled worker has already persisted its outcome before terminating
// the whole group, including descendants that ignore SIGTERM.
func finishUpstreamWorker(ctx context.Context, identity string, result error) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if result != nil || ctx.Err() != nil {
			_, _ = fmt.Fprintln(os.Stderr, errors.Join(result, ctx.Err()))
			return errors.Join(result, ctx.Err(), terminateUpstreamProcess(os.Getpid(), identity, true))
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			result = err
			continue
		}
		children := false
		group := strconv.Itoa(os.Getpid())
		for _, entry := range entries {
			if entry.Name() == group {
				continue
			}
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if err != nil {
				continue
			} // exited or inaccessible unrelated process
			end := strings.LastIndexByte(string(stat), ')')
			if end < 0 {
				continue
			}
			fields := strings.Fields(string(stat[end+1:]))
			if len(fields) >= 4 && fields[0] != "Z" && fields[0] != "X" && fields[2] == group {
				children = true
				break
			}
		}
		if !children {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
