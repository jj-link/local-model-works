// Package backend implements the settings first-party module: the
// operator's view of per-module settings (one JSON object per module,
// stored under optimistic concurrency and addressed by version ETag) and
// the secret store (values sealed with AES-256-GCM before they reach the
// database; the API exposes metadata only and never values). The module
// owns no job kinds and declares no operator settings of its own.

package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the settings backend.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// RegisterJobs: the settings module renders state other modules produce.
func (m *Module) RegisterJobs(*jobs.Registry) {}

// RegisterSettings: the settings module has an empty settingsSchema, so it
// registers nothing.
func (m *Module) RegisterSettings(*settings.Registry) {}

// RegisterHTTP mounts the module's routes on the authenticated group.
func (m *Module) RegisterHTTP(r chi.Router) {
	r.Get("/modules/{id}/settings", m.getModuleSettings)
	r.Put("/modules/{id}/settings", m.putModuleSettings)
	r.Get("/secrets", m.listSecrets)
	r.Post("/secrets", m.putSecret)
	r.Delete("/secrets/{id}", m.deleteSecret)
}

// secretVersion is the authenticated-data version bound into every sealed
// value. The secrets table is unversioned, so each write seals under the
// row's UUID at version 1.
const secretVersion = 1

// purposes mirrors the secrets table CHECK constraint and the fragment's
// SecretWrite.purpose enum.
var purposes = map[string]bool{"huggingface": true, "github": true, "registry": true}

// uuidRe matches the lowercase UUIDv7 ids produced by internal/id (the
// fragment types Secret.id as format: uuid).
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// moduleSettings is components/schemas/ModuleSettings, used both as the
// PUT request body and the GET/PUT response: the stored value plus the
// version ETag for the next conditional write.
type moduleSettings struct {
	Module   string         `json:"module"`
	Settings map[string]any `json:"settings"`
	Version  string         `json:"version"`
}

// secretView is components/schemas/Secret: metadata only, never the
// ciphertext or the opened value.
type secretView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Purpose   string `json:"purpose"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func view(row db.Secret) secretView {
	return secretView{
		ID: row.ID, Name: row.Name, Purpose: row.Purpose,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// secretWrite is components/schemas/SecretWrite.
type secretWrite struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Value   string `json:"value"`
}

// isConstraint matches SQLite constraint violations (modernc.org/sqlite
// reports them as "UNIQUE constraint failed: ..." / "PRIMARY KEY
// constraint failed: ...").
func isConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

// getModuleSettings — GET /modules/{id}/settings: the stored value for
// the module, or the empty object with the initial version "0" when the
// module has never been set. Modules without a registered settings schema
// are 404.
func (m *Module) getModuleSettings(w http.ResponseWriter, r *http.Request) {
	mod := chi.URLParam(r, "id")
	if !m.env.Settings.Has(mod) {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found",
			fmt.Sprintf("module %s has no settings", mod))
		return
	}
	value, version, err := m.env.Settings.Get(r.Context(), mod)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, moduleSettings{
		Module: mod, Settings: value, Version: version,
	})
}

// putModuleSettings — PUT /modules/{id}/settings: the If-Match header
// (required by the fragment) carries the version ETag the client read,
// and the body's version field must agree with it. A first-time set
// matches the initial version "0" that GET returns for an unset module.
func (m *Module) putModuleSettings(w http.ResponseWriter, r *http.Request) {
	mod := chi.URLParam(r, "id")
	if !m.env.Settings.Has(mod) {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found",
			fmt.Sprintf("module %s has no settings", mod))
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			"If-Match header is required")
		return
	}
	var body moduleSettings
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if body.Module != mod {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			fmt.Sprintf("module %q does not match path id %q", body.Module, mod))
		return
	}
	if body.Settings == nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			"settings object is required")
		return
	}
	if body.Version != ifMatch {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			fmt.Sprintf("version %q does not match If-Match %q", body.Version, ifMatch))
		return
	}
	version, err := m.env.Settings.Set(r.Context(), mod, body.Settings, ifMatch)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrUnknownModule):
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", err.Error())
		case errors.Is(err, settings.ErrInvalid):
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		case errors.Is(err, settings.ErrStale):
			httpx.WriteErr(w, http.StatusConflict, "resource.conflict", err.Error())
		default:
			httpx.HandleErr(w, err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, moduleSettings{
		Module: mod, Settings: body.Settings, Version: version,
	})
}

// listSecrets — GET /secrets: secret metadata (id, name, purpose,
// timestamps); the sealed values stay in the database.
func (m *Module) listSecrets(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListSecrets(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]secretView, 0, len(rows))
	for _, row := range rows {
		out = append(out, view(row))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// putSecret — POST /secrets: create a secret, or replace the value of the
// existing secret with the same name (201 in both cases, per the fragment).
func (m *Module) putSecret(w http.ResponseWriter, r *http.Request) {
	var body secretWrite
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if body.Name == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			"name is required")
		return
	}
	if !purposes[body.Purpose] {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			"purpose must be one of huggingface, github, registry")
		return
	}
	out, err := m.setSecret(r.Context(), body)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// setSecret seals the value and creates the secret, or replaces the value
// of the existing secret with the same name. The secrets table is
// unversioned and exposes no UPDATE query: a replace is a transactional
// delete + insert that keeps the row's UUID, so a name's identity is
// stable across writes. A concurrent writer on the same name loses the
// UNIQUE constraint and surfaces as a 409.
func (m *Module) setSecret(ctx context.Context, body secretWrite) (secretView, error) {
	tx, err := m.env.DB.BeginTx(ctx, nil)
	if err != nil {
		return secretView{}, err
	}
	defer tx.Rollback()
	qtx := m.env.Q.WithTx(tx)

	secretID, err := id.New()
	if err != nil {
		return secretView{}, err
	}
	replace := false
	if existing, err := qtx.GetSecretByName(ctx, body.Name); err != nil {
		if !httpx.IsNoRows(err) {
			return secretView{}, err
		}
	} else {
		secretID, replace = existing.ID, true
	}
	nonce, ciphertext, err := m.env.Secrets.Seal(secretID, body.Value, secretVersion)
	if err != nil {
		return secretView{}, fmt.Errorf("seal secret %q: %w", body.Name, err)
	}
	if replace {
		if err := qtx.DeleteSecret(ctx, secretID); err != nil {
			return secretView{}, err
		}
	}
	if err := qtx.CreateSecret(ctx, db.CreateSecretParams{
		ID: secretID, Name: body.Name, Purpose: body.Purpose,
		Nonce: nonce, Ciphertext: ciphertext,
	}); err != nil {
		if isConstraint(err) {
			return secretView{}, fmt.Errorf("%w: secret %q already exists",
				httpx.ErrConflict, body.Name)
		}
		return secretView{}, err
	}
	if err := tx.Commit(); err != nil {
		return secretView{}, err
	}
	row, err := m.env.Q.GetSecret(ctx, secretID)
	if err != nil {
		return secretView{}, err
	}
	return view(row), nil
}

// deleteSecret — DELETE /secrets/{id}: 404 when the id is unknown. Secret
// ids are UUIDs (the fragment types Secret.id as format: uuid), so a
// malformed path parameter is rejected with 422 before the lookup.
func (m *Module) deleteSecret(w http.ResponseWriter, r *http.Request) {
	secretID := chi.URLParam(r, "id")
	if !uuidRe.MatchString(secretID) {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			"secret id must be a UUID")
		return
	}
	ctx := r.Context()
	if _, err := m.env.Q.GetSecret(ctx, secretID); err != nil {
		if httpx.IsNoRows(err) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found",
				fmt.Sprintf("secret %s not found", secretID))
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if err := m.env.Q.DeleteSecret(ctx, secretID); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
