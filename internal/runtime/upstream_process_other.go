//go:build !linux

package runtime

import (
	"context"
	"fmt"
	"os/exec"
)

func configureUpstreamProcess(cmd *exec.Cmd) error { return fmt.Errorf("upstream.linux_required") }
func upstreamProcessIdentity(pid int) (string, error) {
	return "", fmt.Errorf("upstream.linux_required")
}
func upstreamProcessAlive(pid int, identity string) bool { return false }
func terminateUpstreamProcess(pid int, identity string, force bool) error {
	return fmt.Errorf("upstream.linux_required")
}
func finishUpstreamWorker(ctx context.Context, identity string, result error) error {
	return fmt.Errorf("upstream.linux_required")
}
