// Package settings is the module operator-settings SDK: modules declare a
// settings schema at registration (from the manifest's settingsSchema), and
// the settings API stores and validates one JSON object per module under
// optimistic concurrency (version ETag).
package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
)

var (
	ErrUnknownModule = errors.New("module has no settings schema")
	ErrStale         = errors.New("settings version conflict")
	ErrInvalid       = errors.New("settings invalid")
)

// Registry stores and validates per-module operator settings.
type Registry struct {
	q        *db.Queries
	mu       sync.RWMutex
	compiled map[string]*jsonschema.Schema
}

// New builds the settings registry over the core queries.
func New(q *db.Queries) *Registry {
	return &Registry{q: q, compiled: map[string]*jsonschema.Schema{}}
}

// Register declares the settings schema for a module.
func (r *Registry) Register(moduleID string, schema json.RawMessage) error {
	if moduleID == "" {
		return errors.New("module id required")
	}
	var compiled *jsonschema.Schema
	if schema != nil {
		// Decoded before AddResource: the v6 compiler validates resources
		// against the meta-schema and does not decode raw io.Reader
		// documents itself.
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
		if err != nil {
			return fmt.Errorf("module %s settings schema: %v", moduleID, err)
		}
		comp := jsonschema.NewCompiler()
		if err := comp.AddResource("settings://"+moduleID, doc); err != nil {
			return fmt.Errorf("module %s settings schema: %v", moduleID, err)
		}
		s, err := comp.Compile("settings://" + moduleID)
		if err != nil {
			return fmt.Errorf("module %s settings schema: %v", moduleID, err)
		}
		compiled = s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.compiled[moduleID] = compiled
	return nil
}

// Has reports whether a module registered settings.
func (r *Registry) Has(moduleID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.compiled[moduleID]
	return ok
}

// Get returns the stored settings (or the empty object) with its version.
func (r *Registry) Get(ctx context.Context, moduleID string) (map[string]any, string, error) {
	if !r.Has(moduleID) {
		return nil, "", fmt.Errorf("%w: %s", ErrUnknownModule, moduleID)
	}
	row, err := r.q.GetModuleSettings(ctx, moduleID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]any{}, "0", nil
		}
		return nil, "", err
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(row.Settings), &out)
	return out, row.Version, nil
}

// Set validates value against the module schema and stores it when
// ifMatch equals the current version. It returns the new version.
func (r *Registry) Set(ctx context.Context, moduleID string, value map[string]any, ifMatch string) (string, error) {
	r.mu.RLock()
	compiled, ok := r.compiled[moduleID]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownModule, moduleID)
	}
	if compiled != nil {
		if err := compiled.Validate(value); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	cur, err := r.q.GetModuleSettings(ctx, moduleID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		cur = db.ModuleSetting{Module: moduleID, Settings: "{}", Version: "0"}
	}
	if cur.Version != ifMatch {
		return "", fmt.Errorf("%w: have %s, got %s", ErrStale, cur.Version, ifMatch)
	}
	enc, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	version, err := id.New()
	if err != nil {
		return "", err
	}
	if err := r.q.PutModuleSettings(ctx, db.PutModuleSettingsParams{
		Module: moduleID, Settings: string(enc), Version: version,
	}); err != nil {
		return "", err
	}
	return version, nil
}
