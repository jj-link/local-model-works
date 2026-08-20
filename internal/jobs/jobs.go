// Package jobs is the shared one-shot job SDK: modules declare job kinds
// (validated against the manifest's jobKinds), submit typed inputs, and get
// a run row, an isolated per-run workspace, byte-cursor logs, and output
// validation without owning a process manager.
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

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
	Executor func(ctx context.Context, c *Context) (map[string]any, error)
}

// Context is handed to an executor for one run.
type Context struct {
	Module string
	Kind   string
	RunID  string
	Input  map[string]any
	// Workspace is the run's private, isolated directory (created).
	Workspace string
	// Logf appends to the run's standard log stream.
	Logf func(format string, args ...any)
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
}

// New builds a job registry over the run service. lifecycle bounds all
// background job execution and its persistence.
func New(runsSvc *runs.Service, runRoot string, lifecycle context.Context) *Registry {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	return &Registry{
		specs:      map[string]Spec{},
		moduleOf:   map[string]string{},
		live:       map[string]context.CancelFunc{},
		inSchemas:  map[string]*jsonschema.Schema{},
		outSchemas: map[string]*jsonschema.Schema{},
		lc:         lifecycle,
		runs:       runsSvc,
		runRoot:    runRoot,
	}
}

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
		_ = r.runs.SetState(ctx, runID, runs.Failed, "run.workspace_failed", err.Error())
		return "", fmt.Errorf("%w: %v", ErrNoWorkspace, err)
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
		c := &Context{
			Module:    module,
			Kind:      spec.Kind,
			RunID:     runID,
			Input:     input,
			Workspace: ws,
			Logf: func(format string, args ...any) {
				_ = r.runs.AppendLog(runID, "", 0, "stdout", []byte(fmt.Sprintf(format, args...)))
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
					if vErr := outSchema.Validate(out); vErr != nil {
						to, code, msg = runs.Failed, "run.output_invalid", vErr.Error()
					}
				}
			}
			// A cancel already moved the run to cancelling: land there.
			if row, gErr := r.runs.Get(lc, runID); gErr == nil && row.State == string(runs.Cancelling) && !to.Terminal() {
				to, code, msg = runs.Cancelled, "run.cancelled", msg
			}
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
