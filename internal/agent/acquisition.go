package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Attempt records contain no credentials. A stopped process is a quiescence
// barrier, not permission to replay: an old command ID stays interrupted.
type acquisitionAttempt struct {
	ID          string          `json:"id"`
	Identity    string          `json:"identity"`
	Destination string          `json:"destination"`
	Binding     string          `json:"binding"`
	State       string          `json:"state"`
	Error       string          `json:"error,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
	cancel      context.CancelFunc
	done        chan struct{}
}

func (a *Agent) acquisitionPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(a.cfg.StateRoot, "acquisitions", hex.EncodeToString(sum[:])+".json")
}
func (a *Agent) saveAcquisition(at *acquisitionAttempt) error {
	path := a.acquisitionPath(at.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(at)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".part", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".part", path)
}
func (a *Agent) loadAcquisition(id string) (*acquisitionAttempt, error) {
	if a.acquisitions == nil {
		a.acquisitions = map[string]*acquisitionAttempt{}
	}
	if at := a.acquisitions[id]; at != nil {
		return at, nil
	}
	data, err := os.ReadFile(a.acquisitionPath(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var at acquisitionAttempt
	if len(data) > 128<<10 || json.Unmarshal(data, &at) != nil || at.ID != id {
		return nil, fmt.Errorf("download.checkpoint_invalid")
	}
	at.done = make(chan struct{})
	close(at.done)
	if at.State == "active" {
		at.State = "interrupted"
		at.Error = "download.interrupted: explicit Resume required"
	}
	a.acquisitions[id] = &at
	return &at, nil
}
func (a *Agent) beginAcquisition(ctx context.Context, id, identity, destination, binding string) (context.Context, *acquisitionAttempt, bool, error) {
	if id == "" || len(id) > 256 {
		return nil, nil, false, fmt.Errorf("download.command_id_invalid")
	}
	a.acquisitionMu.Lock()
	defer a.acquisitionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if a.acquisitions == nil {
		a.acquisitions = map[string]*acquisitionAttempt{}
	}
	old, err := a.loadAcquisition(id)
	if err != nil {
		return nil, nil, false, err
	}
	if old != nil {
		if old.Identity != identity || old.Destination != destination || old.Binding != binding {
			return nil, nil, false, fmt.Errorf("download.command_conflict")
		}
		return ctx, old, false, nil
	}
	for _, at := range a.acquisitions {
		if at.State == "active" && (pathWithin(destination, at.Destination) || pathWithin(at.Destination, destination)) {
			return nil, nil, false, fmt.Errorf("download.destination_busy")
		}
	}
	commandCtx, cancel := context.WithCancel(ctx)
	at := &acquisitionAttempt{ID: id, Identity: identity, Destination: destination, Binding: binding, State: "active", cancel: cancel, done: make(chan struct{})}
	if err := a.saveAcquisition(at); err != nil {
		cancel()
		return nil, nil, false, err
	}
	a.acquisitions[id] = at
	return commandCtx, at, true, nil
}
func (a *Agent) finishAcquisition(at *acquisitionAttempt, output []byte, err error) {
	a.acquisitionMu.Lock()
	defer a.acquisitionMu.Unlock()
	if at.State != "active" {
		return
	}
	at.State = "succeeded"
	if err != nil {
		at.State = "failed"
		at.Error = err.Error()
	}
	at.Output = append(json.RawMessage(nil), output...)
	if saveErr := a.saveAcquisition(at); saveErr != nil {
		at.State = "failed"
		at.Error = "download.checkpoint_write_failed"
	}
	at.cancel()
	close(at.done)
}
func (a *Agent) cancelAcquisition(ctx context.Context, id string) (*acquisitionAttempt, error) {
	a.acquisitionMu.Lock()
	at, err := a.loadAcquisition(id)
	if err == nil && at == nil {
		err = fmt.Errorf("download.cancel_target_unknown")
	}
	if err == nil && at.cancel != nil {
		at.cancel()
	}
	a.acquisitionMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-at.done:
		return at, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Agent) stopAcquisitions() {
	a.acquisitionMu.Lock()
	var attempts []*acquisitionAttempt
	for _, at := range a.acquisitions {
		if at.State == "active" {
			at.cancel()
			attempts = append(attempts, at)
		}
	}
	a.acquisitionMu.Unlock()
	for _, at := range attempts {
		<-at.done
	}
}

// Existing parents may be absent; every existing component must be a real
// directory (the destination itself may be a regular file). Never follow a
// caller-controlled symlink into another cache or a mounted workload tree.
func safeDestination(destination string) error {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return fmt.Errorf("download.destination_invalid")
	}
	for p := destination; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("download.destination_symlink")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}
