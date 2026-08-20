// Package jobs is the shared one-shot job SDK: modules declare job kinds
// (validated against the manifest's jobKinds), submit typed inputs, and get
// a run row, an isolated per-run workspace, byte-cursor logs, and output
// validation without owning a process manager.
package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/runs"
)

var (
	ErrUnknownKind = errors.New("unknown job kind")
	ErrInput       = errors.New("job input invalid")
	ErrSchema      = errors.New("job schema invalid")
	ErrNoWorkspace = errors.New("job workspace unavailable")
)

// Spec declares one job kind.
type Spec struct {
	Kind string
	// Title is the human label used in run listings.
	Title string
	// InputSchema is a JSON Schema (draft 2020-12) the submission input is
	// validated against before a run is created.
	InputSchema json.RawMessage
	// OutputSchema, when set, validates the executor's output object before
	// the run is marked succeeded.
	OutputSchema json.RawMessage
	// Schedule, when nonempty, runs the job on that interval (e.g. "5m")
	// with an empty input object.
	Schedule string
	// Executor runs the job. It must respect ctx cancellation and return a
	// versioned JSON-ready output object.
	Executor              func(ctx context.Context, c *Context) (map[string]any, error)
	SecretScopes          []string
	PlacementRequirements []string
	LeaseResources        func(input map[string]any) []string
	ArtifactKinds         []string
}

// Context is handed to an executor for one run.
type Context struct {
	Module string
	Kind   string
	RunID  string
	Input  map[string]any
	// Workspace is the run's private, isolated directory.
	Workspace       string
	Logf            func(format string, args ...any)
	Secrets         map[string]string
	PublishArtifact func(kind, path string) (PublishedArtifact, error)
	Placements      []string
}
type PublishedArtifact struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

// Registry is the module job registry. All asynchronous run persistence
// uses the service-lifecycle context captured at construction, never a
// request context.
type Registry struct {
	mu         sync.RWMutex
	specs      map[string]Spec
	moduleOf   map[string]string
	live       map[string]context.CancelFunc
	inSchemas  map[string]*jsonschema.Schema
	outSchemas map[string]*jsonschema.Schema

	lc      context.Context // service lifecycle; not a request context
	runs    *runs.Service
	runRoot string
	db      *sql.DB
	q       *db.Queries
	secrets *auth.SecretBox
}

// New builds a job registry over the run service. lifecycle bounds all
// background job execution and its persistence.
func New(runsSvc *runs.Service, runRoot string, lifecycle context.Context, sqlDB *sql.DB, queries *db.Queries) *Registry {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	return &Registry{
		specs: map[string]Spec{}, moduleOf: map[string]string{}, live: map[string]context.CancelFunc{},
		inSchemas: map[string]*jsonschema.Schema{}, outSchemas: map[string]*jsonschema.Schema{},
		lc: lifecycle, runs: runsSvc, runRoot: runRoot, db: sqlDB, q: queries,
	}
}

func (r *Registry) SetSecretBox(box *auth.SecretBox) { r.secrets = box }

// compileSchema compiles one raw JSON Schema document. The document is
// decoded before AddResource: the v6 compiler validates resources against
// the meta-schema and does not decode raw io.Reader documents itself.
func compileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	comp := jsonschema.NewCompiler()
	if err := comp.AddResource(name, doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	s, err := comp.Compile(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	return s, nil
}

// Register declares a job kind owned by moduleID. Duplicate kinds are a
// wiring error.
func (r *Registry) Register(moduleID string, spec Spec) error {
	if spec.Kind == "" || spec.Executor == nil {
		return fmt.Errorf("job spec %q: kind and executor required", spec.Kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.specs[spec.Kind]; ok {
		return fmt.Errorf("job kind %q registered twice", spec.Kind)
	}
	if spec.InputSchema != nil {
		s, err := compileSchema("job://"+spec.Kind+"/input", spec.InputSchema)
		if err != nil {
			return fmt.Errorf("job %s input schema: %v", spec.Kind, err)
		}
		r.inSchemas[spec.Kind] = s
	}
	if spec.OutputSchema != nil {
		s, err := compileSchema("job://"+spec.Kind+"/output", spec.OutputSchema)
		if err != nil {
			return fmt.Errorf("job %s output schema: %v", spec.Kind, err)
		}
		r.outSchemas[spec.Kind] = s
	}
	r.specs[spec.Kind] = spec
	r.moduleOf[spec.Kind] = moduleID
	return nil
}

// Lookup returns a declared job kind.
func (r *Registry) Lookup(kind string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[kind]
	return s, ok
}

// Kinds lists declared job kinds sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.specs))
	for k := range r.specs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normalizeJSON(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

// Submit validates the input, creates a run, and starts the executor. The
// request ctx only covers creation; execution outlives the request.
func (r *Registry) Submit(ctx context.Context, kind string, input map[string]any) (string, error) {
	r.mu.RLock()
	spec, ok := r.specs[kind]
	module := r.moduleOf[kind]
	in, hasIn := r.inSchemas[kind]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	if input == nil {
		input = map[string]any{}
	}
	normalized, err := normalizeJSON(input)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInput, err)
	}
	input = normalized.(map[string]any)
	if hasIn {
		if err := in.Validate(input); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInput, err)
		}
	}
	runID, err := r.runs.Create(ctx, module, kind, input, "")
	if err != nil {
		return "", err
	}
	ws := filepath.Join(r.runRoot, "jobs", runID)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		_ = r.runs.Complete(ctx, runID, runs.Failed, "run.workspace_failed", err.Error())
		return "", fmt.Errorf("%w: %v", ErrNoWorkspace, err)
	}
	if spec.LeaseResources != nil {
		resources := spec.LeaseResources(input)
		if len(resources) > 0 {
			tx, err := r.db.BeginTx(ctx, nil)
			if err != nil {
				return "", err
			}
			if err := r.runs.AcquireLeases(ctx, r.q.WithTx(tx), "run", runID, resources); err != nil {
				tx.Rollback()
				_ = r.runs.Complete(ctx, runID, runs.Failed, "run.lease_conflict", err.Error())
				return "", err
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
		}
	}
	r.start(module, spec, runID, ws, input)
	return runID, nil
}

func (r *Registry) start(module string, spec Spec, runID, ws string, input map[string]any) {
	lc := r.lc
	execCtx, cancel := context.WithCancel(lc)
	r.mu.Lock()
	r.live[runID] = cancel
	r.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			delete(r.live, runID)
			r.mu.Unlock()
		}()
		secrets, secretErr := r.loadSecrets(execCtx, spec.SecretScopes)
		if secretErr != nil {
			_ = r.runs.ReleaseLeasesFor(lc, "run", runID)
			_ = r.runs.Complete(lc, runID, runs.Failed, "run.secret_scope", secretErr.Error())
			return
		}
		defer func() {
			for key := range secrets {
				delete(secrets, key)
			}
		}()
		allowedKinds := map[string]bool{}
		for _, kind := range spec.ArtifactKinds {
			allowedKinds[kind] = true
		}
		c := &Context{
			Module:    module,
			Kind:      spec.Kind,
			RunID:     runID,
			Input:     input,
			Workspace: ws,
			Logf: func(format string, args ...any) {
				_ = r.runs.AppendLog(runID, "", 0, "stdout", []byte(fmt.Sprintf(format, args...)))
			},
			Secrets:    secrets,
			Placements: append([]string(nil), spec.PlacementRequirements...),
			PublishArtifact: func(kind, path string) (PublishedArtifact, error) {
				if !allowedKinds[kind] {
					return PublishedArtifact{}, fmt.Errorf("artifact kind %q is not declared", kind)
				}
				return r.publishArtifact(execCtx, runID, ws, kind, path)
			},
		}
		_ = r.runs.SetState(lc, runID, runs.Running, "", "")
		out, err := spec.Executor(execCtx, c)

		finish := func(to runs.State, code, msg string) {
			if to == runs.Succeeded && out != nil {
				r.mu.RLock()
				outSchema := r.outSchemas[spec.Kind]
				r.mu.RUnlock()
				if outSchema != nil {
					normalized, normalizeErr := normalizeJSON(out)
					if normalizeErr != nil {
						to, code, msg = runs.Failed, "run.output_invalid", normalizeErr.Error()
					} else if vErr := outSchema.Validate(normalized); vErr != nil {
						to, code, msg = runs.Failed, "run.output_invalid", vErr.Error()
					}
				}
			}
			// A cancel already moved the run to cancelling: land there.
			if row, gErr := r.runs.Get(lc, runID); gErr == nil && row.State == string(runs.Cancelling) && !to.Terminal() {
				to, code = runs.Cancelled, "run.cancelled"
			}
			_ = r.runs.ReleaseLeasesFor(lc, "run", runID)
			_ = r.runs.Complete(lc, runID, to, code, msg)
		}
		if err != nil {
			if execCtx.Err() != nil {
				finish(runs.Cancelled, "run.cancelled", "job cancelled")
			} else {
				finish(runs.Failed, "run.failed", err.Error())
			}
			return
		}
		_ = r.runs.SetState(lc, runID, runs.Verifying, "", "")
		finish(runs.Succeeded, "", "")
	}()
}

func (r *Registry) loadSecrets(ctx context.Context, scopes []string) (map[string]string, error) {
	values := map[string]string{}
	if len(scopes) == 0 {
		return values, nil
	}
	if r.secrets == nil || r.q == nil {
		return nil, fmt.Errorf("secret store is unavailable")
	}
	for _, name := range scopes {
		secret, err := r.q.GetSecretByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("secret scope %s: %w", name, err)
		}
		value, err := r.secrets.Open(secret.ID, 1, secret.Nonce, secret.Ciphertext)
		if err != nil {
			return nil, err
		}
		values[name] = value
	}
	return values, nil
}

func (r *Registry) publishArtifact(ctx context.Context, runID, workspace, kind, requestedPath string) (PublishedArtifact, error) {
	path := requestedPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	relative, err := filepath.Rel(workspace, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return PublishedArtifact{}, fmt.Errorf("artifact path escapes workspace")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return PublishedArtifact{}, fmt.Errorf("published artifact must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return PublishedArtifact{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	file.Close()
	if err != nil {
		return PublishedArtifact{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	identity := "file://" + digest
	sum := sha256.Sum256([]byte(identity))
	artifactID := "artifact-" + hex.EncodeToString(sum[:8])
	metadata, _ := json.Marshal(map[string]string{"run_id": runID, "path": path})
	if err := r.q.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: artifactID, Kind: kind, Identity: identity,
		Digest: sql.NullString{String: digest, Valid: true}, Metadata: string(metadata),
	}); err != nil {
		return PublishedArtifact{}, err
	}
	return PublishedArtifact{ID: artifactID, Kind: kind, Identity: identity, Path: path, Size: size}, nil
}

// Cancel stops a live job's context and records the cancelling state.
func (r *Registry) Cancel(ctx context.Context, runID string) error {
	r.mu.RLock()
	cancel, live := r.live[runID]
	r.mu.RUnlock()
	if live {
		cancel()
	}
	return r.runs.Cancel(ctx, runID)
}

// StartSchedules launches the interval loop for scheduled job kinds. The
// loops stop when the service-lifecycle context is done.
func (r *Registry) StartSchedules() {
	r.mu.RLock()
	var kinds []string
	for k, s := range r.specs {
		if s.Schedule != "" {
			kinds = append(kinds, k)
		}
	}
	r.mu.RUnlock()
	sort.Strings(kinds)
	for _, k := range kinds {
		spec, _ := r.Lookup(k)
		interval, err := time.ParseDuration(spec.Schedule)
		if err != nil || interval <= 0 {
			continue
		}
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-r.lc.Done():
					return
				case <-t.C:
					_, _ = r.Submit(r.lc, k, map[string]any{})
				}
			}
		}()
	}
}
