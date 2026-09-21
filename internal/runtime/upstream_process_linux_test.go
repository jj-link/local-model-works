//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUpstreamProcessGroupCancellationChecksIdentity(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 60 & printf '%s\\n' \"$!\" > \"$1\"; wait", "fixture", pidFile)
	if err := configureUpstreamProcess(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := upstreamProcessIdentity(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	defer func() { _ = terminateUpstreamProcess(cmd.Process.Pid, identity, true); _ = cmd.Wait() }()
	var child int
	deadline := time.Now().Add(3 * time.Second)
	for child == 0 {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture child did not start")
		}
		if child == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	childIdentity, err := upstreamProcessIdentity(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminateUpstreamProcess(cmd.Process.Pid, identity+"-not-this-process", true); err != nil {
		t.Fatal(err)
	}
	if !upstreamProcessAlive(cmd.Process.Pid, identity) || !upstreamProcessAlive(child, childIdentity) {
		t.Fatal("stale process identity killed a live process")
	}
	if err := terminateUpstreamProcess(cmd.Process.Pid, identity, true); err != nil {
		t.Fatal(err)
	}
	for upstreamProcessAlive(child, childIdentity) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if upstreamProcessAlive(cmd.Process.Pid, identity) || upstreamProcessAlive(child, childIdentity) {
		t.Fatal("forced cancellation left part of the process group alive")
	}
}
