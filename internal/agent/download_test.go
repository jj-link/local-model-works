package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type downloadTestRuntime struct {
	ownershipRuntime
	mu        sync.Mutex
	image     *runtime.ImageInfo
	pulls     int
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (r *downloadTestRuntime) Pull(ctx context.Context, spec *runtime.PullSpec) error {
	r.mu.Lock()
	r.pulls++
	r.mu.Unlock()
	if r.entered != nil {
		close(r.entered)
		<-ctx.Done()
		close(r.cancelled)
		<-r.release
		return ctx.Err()
	}
	_, digest, _ := strings.Cut(spec.Reference, "@")
	r.mu.Lock()
	r.image = &runtime.ImageInfo{Reference: spec.Reference, Digest: digest, IndexDigest: digest, ManifestDigest: digest, Platform: spec.Platform, SizeBytes: 123}
	r.mu.Unlock()
	if spec.Progress != nil {
		spec.Progress(runtime.ImagePullProgress{Layer: digest, Phase: "complete", BytesDone: 123, BytesTotal: 123})
	}
	return nil
}
func (r *downloadTestRuntime) InspectImage(ctx context.Context, reference, platform string) (*runtime.ImageInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.image == nil {
		return nil, fmt.Errorf("image.not_found")
	}
	if r.image.Reference != reference {
		return nil, fmt.Errorf("image.not_found")
	}
	if r.image.Platform != platform {
		return nil, fmt.Errorf("image.platform_mismatch")
	}
	copy := *r.image
	return &copy, nil
}
func imageDownloadCommand(t *testing.T, id string) *agentv1.DownloadCommand {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	spec := downloads.ResourceSpec{Kind: downloads.ResourceImage, Identity: "registry.example/team/image@" + digest, Source: downloads.SourceSpec{Type: downloads.SourceOCI, Reference: "registry.example/team/image", Digest: digest}, Destination: "/var/lib/docker", Platform: "linux/amd64", IndexDigest: digest, ManifestDigest: digest}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &agentv1.DownloadCommand{CommandId: id, ItemId: "item-" + id, Op: agentv1.DownloadOp_DOWNLOAD_OP_FETCH, ResourceJson: raw}
}
func awaitDownloadResult(t *testing.T, a *Agent, id string) *agentv1.CommandResult {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-a.sendQ:
			if result := message.GetCommandResult(); result != nil && result.GetCommandId() == id {
				return result
			}
		case <-timer.C:
			t.Fatalf("no command result for %s", id)
			return nil
		}
	}
}
func TestDownloadImageNeverCreatesOrStartsAContainer(t *testing.T) {
	rt := &downloadTestRuntime{}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	command := imageDownloadCommand(t, "fetch")
	a.handleDownload(t.Context(), command)
	result := awaitDownloadResult(t, a, "fetch")
	if !result.GetOk() {
		t.Fatal(result.GetError())
	}
	var output downloads.CommandOutput
	if err := json.Unmarshal(result.GetOutputJson(), &output); err != nil {
		t.Fatal(err)
	}
	if output.State != downloads.ResourceAvailable || output.Platform != "linux/amd64" || output.VerifiedAt == nil {
		t.Fatalf("unverified completion: %+v", output)
	}
	if rt.pulls != 1 || len(rt.calls) != 0 {
		t.Fatalf("download executed workload actions: pulls=%d calls=%v", rt.pulls, rt.calls)
	}
	// Duplicate delivery replays the terminal observation, never a second pull.
	a.handleDownload(t.Context(), command)
	if !awaitDownloadResult(t, a, "fetch").GetOk() || rt.pulls != 1 {
		t.Fatal("duplicate command started a new acquisition")
	}
}

func TestDownloadImageRecognizesPinnedIndexAndRejectsWrongPlatformManifest(t *testing.T) {
	index := "sha256:" + strings.Repeat("a", 64)
	child := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name             string
		observedManifest string
		expectedManifest string
		want             downloads.ResourceState
	}{
		{"cached index contains exact platform", child, child, downloads.ResourceAvailable},
		{"cached index contains a different platform manifest", "sha256:" + strings.Repeat("c", 64), child, downloads.ResourceInvalid},
		{"classic store verifies a single manifest", "", index, downloads.ResourceAvailable},
		{"classic index cannot prove an absent child", "", child, downloads.ResourceMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &downloadTestRuntime{image: &runtime.ImageInfo{
				Reference: "registry.example/team/image@" + index,
				Digest:    index, IndexDigest: index, ManifestDigest: tc.observedManifest,
				Platform: "linux/amd64", SizeBytes: 123,
			}}
			if tc.observedManifest == "" {
				rt.image.IndexDigest = ""
			}
			a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
			command := imageDownloadCommand(t, "inspect")
			command.Op = agentv1.DownloadOp_DOWNLOAD_OP_INSPECT
			var spec downloads.ResourceSpec
			if err := json.Unmarshal(command.ResourceJson, &spec); err != nil {
				t.Fatal(err)
			}
			spec.ManifestDigest = tc.expectedManifest
			var err error
			command.ResourceJson, err = json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			a.handleDownload(t.Context(), command)
			result := awaitDownloadResult(t, a, "inspect")
			if !result.GetOk() {
				t.Fatal(result.GetError())
			}
			var output downloads.CommandOutput
			if err := json.Unmarshal(result.GetOutputJson(), &output); err != nil {
				t.Fatal(err)
			}
			if output.State != tc.want || rt.pulls != 0 {
				t.Fatalf("cached index verification = %+v, pulls=%d", output, rt.pulls)
			}
			if tc.want == downloads.ResourceAvailable && (output.VerifiedAt == nil || time.Since(*output.VerifiedAt) > 2*time.Minute) {
				t.Fatalf("available image cannot pass controller freshness validation: %+v", output)
			}
		})
	}
}
func TestDownloadCancelWaitsForWriterQuiescence(t *testing.T) {
	rt := &downloadTestRuntime{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	command := imageDownloadCommand(t, "fetch")
	go a.handleDownload(t.Context(), command)
	select {
	case <-rt.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("pull did not start")
	}
	go a.handleDownload(t.Context(), command)
	go a.handleDownload(t.Context(), &agentv1.DownloadCommand{CommandId: "cancel", ItemId: command.ItemId, Op: agentv1.DownloadOp_DOWNLOAD_OP_CANCEL, TargetCommandId: command.CommandId})
	select {
	case <-rt.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not receive cancellation")
	}
	for draining := true; draining; {
		select {
		case message := <-a.sendQ:
			if result := message.GetCommandResult(); result != nil && result.CommandId == "cancel" {
				t.Fatal("cancellation acknowledged before writer stopped")
			}
		default:
			draining = false
		}
	}
	close(rt.release)
	if result := awaitDownloadResult(t, a, "cancel"); !result.GetOk() {
		t.Fatal(result.GetError())
	}
	rt.mu.Lock()
	pulls := rt.pulls
	rt.mu.Unlock()
	if pulls != 1 {
		t.Fatalf("duplicate delivery created %d writers", pulls)
	}
}
func TestDownloadRestartDoesNotReplayAnInterruptedCommand(t *testing.T) {
	root := t.TempDir()
	rt := &downloadTestRuntime{}
	a := New(config.Agent{StateRoot: root}, "test", "test", rt, nil)
	command := imageDownloadCommand(t, "fetch")
	spec, err := downloads.DecodeResourceSpec(command.ResourceJson)
	if err != nil {
		t.Fatal(err)
	}
	// This is the durable pre-dispatch state a process crash leaves behind.
	binding := fmt.Sprintf("%s:%d:%x", command.ItemId, command.Op, sha256.Sum256(command.ResourceJson))
	_, at, _, err := a.beginAcquisition(t.Context(), command.CommandId, spec.Identity, spec.Destination, binding)
	if err != nil {
		t.Fatal(err)
	}
	at.cancel()
	restarted := New(config.Agent{StateRoot: root}, "test", "test", rt, nil)
	restarted.handleDownload(t.Context(), command)
	if result := awaitDownloadResult(t, restarted, "fetch"); result.GetOk() {
		t.Fatal("interrupted command was silently replayed")
	}
	if rt.pulls != 0 {
		t.Fatal("restart performed a network acquisition")
	}
	restarted.handleDownload(t.Context(), &agentv1.DownloadCommand{CommandId: "cancel", ItemId: command.ItemId, Op: agentv1.DownloadOp_DOWNLOAD_OP_CANCEL, TargetCommandId: command.CommandId})
	if result := awaitDownloadResult(t, restarted, "cancel"); !result.GetOk() {
		t.Fatal("stopped process did not establish quiescence")
	}
}
