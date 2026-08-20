// Package backend implements the library first-party module: the
// operator's installed-recipe store (list, import, detail with side-by-side
// versions, trust transitions, uninstall), artifact placements, and peer
// transfers between nodes. Recipe import and transfer dispatch run in the
// core recipe and deploy services; this module renders and commands them.

package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the library backend.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// importRequest is the fragment's RecipeImport body: the source of the
// recipe to install.
type importRequest struct {
	Source recipe.RecipeSource `json:"source"`
}

// importInputSchema is the recipe-import job input: the fragment's
// RecipeImport request shape (the source; the service derives trust per
// source type).
var importInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["source"],
  "additionalProperties": false,
  "properties": {
    "source": {
      "type": "object",
      "required": ["type"],
      "additionalProperties": false,
      "properties": {
        "type":      { "type": "string", "enum": ["catalog", "oci", "git", "local"] },
        "reference": { "type": "string" },
        "revision":  { "type": "string" },
        "path":      { "type": "string" },
        "remote":    { "type": "string" },
        "tree":      { "type": "string" }
      }
    }
  }
}`)

// importOutputSchema is the import's confirmation: the stored recipe view.
var importOutputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["digest", "name", "version", "trust_state"],
  "properties": {
    "digest":      { "type": "string" },
    "name":        { "type": "string" },
    "version":     { "type": "string" },
    "trust_state": { "type": "string", "enum": ["verified", "local", "untrusted"] }
  }
}`)

// RegisterJobs declares the recipe-import job: the same import path as the
// synchronous POST /recipes/import handler (shared importRecipe). A
// registration failure means the frozen manifest schema failed to compile
// — a wiring bug, not an operator condition.
func (m *Module) RegisterJobs(reg *jobs.Registry) {
	if err := reg.Register("library", jobs.Spec{
		Kind:         "recipe-import",
		Title:        "Import recipe",
		InputSchema:  importInputSchema,
		OutputSchema: importOutputSchema,
		Executor:     m.importJob,
	}); err != nil {
		panic(fmt.Sprintf("library jobs: %v", err))
	}
}

// importJob runs one recipe import as a ledgered job.
func (m *Module) importJob(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var req importRequest
	b, err := json.Marshal(c.Input)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("recipe-import input: %w", err)
	}
	c.Logf("importing recipe from %s source %q", req.Source.Type, req.Source.Reference)
	rec, err := importRecipe(ctx, m.env, req)
	if err != nil {
		return nil, err
	}
	c.Logf("imported recipe %s@%s (trust %s, digest %s)", rec.Name, rec.Version, rec.TrustState, rec.Digest)
	out := map[string]any{}
	if err := json.Unmarshal(httpx.MustJSON(rec), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RegisterSettings declares the operator settings (catalog URLs, local
// auto-trust) from the manifest's frozen schema; a compile failure is a
// wiring bug.
func (m *Module) RegisterSettings(reg *settings.Registry) {
	if err := reg.Register("library", descriptor.SettingsSchema); err != nil {
		panic(fmt.Sprintf("library settings: %v", err))
	}
}

// RegisterHTTP mounts the module's routes on the authenticated group.
func (m *Module) RegisterHTTP(r chi.Router) {
	r.Get("/recipes", m.listRecipes)
	r.Post("/recipes/import", m.importRecipeHandler)
	r.Get("/recipes/{digest}", m.getRecipe)
	r.Delete("/recipes/{digest}", m.deleteRecipe)
	r.Post("/recipes/{digest}/trust", m.setRecipeTrust)
	r.Get("/artifacts", m.listArtifacts)
	r.Get("/artifacts/{id}/placements", m.listArtifactPlacements)
	r.Post("/transfers", m.createTransfer)
	r.Get("/transfers", m.listTransfers)
	r.Get("/transfers/{id}", m.getTransfer)
	r.Delete("/transfers/{id}", m.cancelTransfer)
}

// importRecipe installs one recipe from its source. It is shared by the
// synchronous POST handler and the recipe-import job executor so both
// paths store under identical rules. Trust is service-set per source type
// (catalog and local default to local; OCI is verified when the package
// signature validates; git is untrusted); the operator setting
// auto_trust_local=false stores local:// imports untrusted instead.
func importRecipe(ctx context.Context, env *moduleapi.Env, input importRequest) (recipe.Recipe, error) {
	rec, err := env.Recipes.Import(ctx, input.Source)
	if err != nil {
		return recipe.Recipe{}, err
	}
	if input.Source.Type == "local" && !autoTrustLocal(ctx, env) && rec.TrustState == recipe.TrustLocal {
		return env.Recipes.SetTrust(ctx, rec.Digest, recipe.TrustUntrusted, false)
	}
	return rec, nil
}

// autoTrustLocal reports whether local:// imports auto-trust as local.
// The service default is trust; only an explicit false opts out.
func autoTrustLocal(ctx context.Context, env *moduleapi.Env) bool {
	vs, _, err := env.Settings.Get(ctx, "library")
	if err != nil {
		return true
	}
	b, ok := vs["auto_trust_local"].(bool)
	return !ok || b
}

// mapRecipeError converts the recipe service's domain errors to the
// fragment's stable status codes. The service reuses ErrUnknown for a few
// distinct conditions; the message disambiguates them.
func mapRecipeError(err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, recipe.ErrDiffPending), errors.Is(err, recipe.ErrReference):
		return fmt.Errorf("%w: %s", httpx.ErrConflict, msg)
	case strings.Contains(msg, "If-Match"):
		return fmt.Errorf("%w: %s", httpx.ErrConflict, msg)
	case errors.Is(err, recipe.ErrUnpinnedRevision):
		return fmt.Errorf("%w: %s", httpx.ErrUnprocessable, msg)
	case errors.Is(err, recipe.ErrTrustState):
		return fmt.Errorf("%w: %s", httpx.ErrUnprocessable, msg)
	case errors.Is(err, recipe.ErrUnknown) && strings.Contains(msg, "catalog is not configured"):
		return fmt.Errorf("%w: %s", httpx.ErrUnavailable, msg)
	case errors.Is(err, recipe.ErrUnknown), strings.Contains(msg, "source directory:"):
		return fmt.Errorf("%w: %s", httpx.ErrNotFound, msg)
	default:
		return err
	}
}

// listRecipes — GET /recipes: all installed recipes, newest first.
func (m *Module) listRecipes(w http.ResponseWriter, r *http.Request) {
	items, err := m.env.Recipes.List(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

// importRecipeHandler — POST /recipes/import: install from catalog, OCI
// reference, or pinned Git source (201 with the stored recipe).
func (m *Module) importRecipeHandler(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	rec, err := importRecipe(r.Context(), m.env, req)
	if err != nil {
		httpx.HandleErr(w, mapRecipeError(err))
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rec)
}

// recipeDetailView is the fragment's RecipeDetail: the service's detail
// plus the installed side-by-side versions of the same recipe name.
type recipeDetailView struct {
	recipe.RecipeDetail
	Updates []updateEntry `json:"updates,omitempty"`
}

// updateEntry is one side-by-side version with a difference summary.
type updateEntry struct {
	Digest  string         `json:"digest"`
	Version string         `json:"version"`
	Diff    map[string]any `json:"diff"`
}

// getRecipe — GET /recipes/{digest}: full manifest plus update view.
func (m *Module) getRecipe(w http.ResponseWriter, r *http.Request) {
	detail, err := m.env.Recipes.Get(r.Context(), chi.URLParam(r, "digest"))
	if err != nil {
		httpx.HandleErr(w, mapRecipeError(err))
		return
	}
	out := recipeDetailView{RecipeDetail: detail}
	if ups, uerr := m.updates(r.Context(), detail); uerr != nil {
		httpx.HandleErr(w, uerr)
		return
	} else {
		out.Updates = ups
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// updates renders the other installed versions of the same recipe name,
// most advanced first, each with a manifest/permission difference summary
// against the installed digest.
func (m *Module) updates(ctx context.Context, current recipe.RecipeDetail) ([]updateEntry, error) {
	rows, err := m.env.Q.ListRecipeVersions(ctx, current.Name)
	if err != nil {
		return nil, err
	}
	out := make([]updateEntry, 0, len(rows))
	for _, row := range rows {
		if row.Digest == current.Digest {
			continue
		}
		risk, artifactCount, ok := manifestSummary(row.Manifest)
		if !ok {
			continue
		}
		out = append(out, updateEntry{
			Digest:  row.Digest,
			Version: row.Version,
			Diff: map[string]any{
				"version":           map[string]any{"installed": current.Version, "candidate": row.Version},
				"high_risk_added":   diffLists(risk, current.HighRisk),
				"high_risk_removed": diffLists(current.HighRisk, risk),
				"artifact_count":    map[string]any{"installed": current.ArtifactCount, "candidate": artifactCount},
			},
		})
	}
	// Version descending; ties keep the query's installed_at DESC order.
	sort.SliceStable(out, func(i, j int) bool {
		return semverCmp(out[i].Version, out[j].Version) > 0
	})
	return out, nil
}

// deleteRecipe — DELETE /recipes/{digest}: uninstall, blocked while any
// deployment or run references it. If-Match must carry the digest.
func (m *Module) deleteRecipe(w http.ResponseWriter, r *http.Request) {
	digest := chi.URLParam(r, "digest")
	if r.Header.Get("If-Match") == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "invalid.if_match", "If-Match header is required")
		return
	}
	if err := m.env.Recipes.Delete(r.Context(), digest, r.Header.Get("If-Match")); err != nil {
		httpx.HandleErr(w, mapRecipeError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// trustRequest is the fragment's RecipeTrustRequest body.
type trustRequest struct {
	TrustState             string `json:"trust_state"`
	PermissionDiffAccepted bool   `json:"permission_diff_accepted"`
}

// setRecipeTrust — POST /recipes/{digest}/trust: operator trust
// transition (local requires an accepted permission diff).
func (m *Module) setRecipeTrust(w http.ResponseWriter, r *http.Request) {
	var req trustRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if req.TrustState == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "trust_state is required")
		return
	}
	rec, err := m.env.Recipes.SetTrust(r.Context(), chi.URLParam(r, "digest"), req.TrustState, req.PermissionDiffAccepted)
	if err != nil {
		httpx.HandleErr(w, mapRecipeError(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

// artifactView is the fragment's Artifact (metadata and placements
// rendered as objects).
type artifactView struct {
	ID              string          `json:"id"`
	Kind            string          `json:"kind"`
	Identity        string          `json:"identity"`
	Revision        *string         `json:"revision"`
	Digest          *string         `json:"digest"`
	ValidationState string          `json:"validation_state"`
	Metadata        map[string]any  `json:"metadata"`
	Placements      []placementView `json:"placements"`
}

// placementView is the fragment's Placement.
type placementView struct {
	ArtifactID  string            `json:"artifact_id"`
	NodeID      string            `json:"node_id"`
	Path        string            `json:"path"`
	State       string            `json:"state"`
	VerifiedAt  *string           `json:"verified_at"`
	Diagnostics []diag.Diagnostic `json:"diagnostics"`
}

// toArtifactView renders one artifact row with its placements.
func toArtifactView(a db.Artifact, placements []db.ArtifactPlacement) artifactView {
	meta := map[string]any{}
	_ = json.Unmarshal([]byte(a.Metadata), &meta)
	v := artifactView{
		ID: a.ID, Kind: a.Kind, Identity: a.Identity,
		Revision: nullStrPtr(a.Revision), Digest: nullStrPtr(a.Digest),
		ValidationState: a.ValidationState, Metadata: meta,
		Placements: make([]placementView, 0, len(placements)),
	}
	for _, p := range placements {
		v.Placements = append(v.Placements, toPlacementView(p))
	}
	return v
}

// toPlacementView renders one placement row.
func toPlacementView(p db.ArtifactPlacement) placementView {
	ds := diag.Decode(p.Diagnostics)
	if ds == nil {
		ds = []diag.Diagnostic{}
	}
	return placementView{
		ArtifactID: p.ArtifactID, NodeID: p.NodeID, Path: p.Path, State: p.State,
		VerifiedAt: nullStrPtr(p.VerifiedAt), Diagnostics: ds,
	}
}

// listArtifacts — GET /artifacts?kind=&node=: artifacts with placements,
// optionally filtered by kind and/or a placement on one node.
func (m *Module) listArtifacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, node := q.Get("kind"), q.Get("node")
	var (
		rows []db.Artifact
		err  error
	)
	switch {
	case node != "":
		rows, err = m.env.Q.ListArtifactsOnNode(r.Context(), node)
		if err == nil && kind != "" {
			filtered := rows[:0]
			for _, a := range rows {
				if a.Kind == kind {
					filtered = append(filtered, a)
				}
			}
			rows = filtered
		}
	case kind != "":
		rows, err = m.env.Q.ListArtifacts(r.Context(), db.ListArtifactsParams{Column1: kind, Kind: kind})
	default:
		rows, err = m.env.Q.ListArtifacts(r.Context(), db.ListArtifactsParams{Column1: nil, Kind: ""})
	}
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]artifactView, 0, len(rows))
	for _, a := range rows {
		pls, perr := m.env.Q.ListPlacements(r.Context(), a.ID)
		if perr != nil {
			httpx.HandleErr(w, perr)
			return
		}
		out = append(out, toArtifactView(a, pls))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// listArtifactPlacements — GET /artifacts/{id}/placements.
func (m *Module) listArtifactPlacements(w http.ResponseWriter, r *http.Request) {
	aid := chi.URLParam(r, "id")
	if _, err := m.env.Q.GetArtifact(r.Context(), aid); err != nil {
		if httpx.IsNoRows(err) {
			httpx.HandleErr(w, httpx.ErrNotFound)
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	pls, err := m.env.Q.ListPlacements(r.Context(), aid)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]placementView, 0, len(pls))
	for _, p := range pls {
		out = append(out, toPlacementView(p))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// transferRequest is the fragment's TransferRequest body.
type transferRequest struct {
	ArtifactID string `json:"artifact_id"`
	SourceNode string `json:"source_node"`
	DestNode   string `json:"dest_node"`
	DestPath   string `json:"dest_path"`
}

// transferView is the fragment's Transfer (the credential hash and
// updated_at are internal to the core).
type transferView struct {
	ID         string           `json:"id"`
	ArtifactID string           `json:"artifact_id"`
	SourceNode string           `json:"source_node"`
	DestNode   string           `json:"dest_node"`
	DestPath   string           `json:"dest_path"`
	State      string           `json:"state"`
	BytesTotal int64            `json:"bytes_total"`
	BytesDone  int64            `json:"bytes_done"`
	Diagnostic *diag.Diagnostic `json:"diagnostic,omitempty"`
	CreatedAt  string           `json:"created_at"`
}

// toTransferView renders one transfers row; the free-text diagnostic
// column becomes a Diagnostic object.
func toTransferView(t db.Transfer) transferView {
	v := transferView{
		ID: t.ID, ArtifactID: t.ArtifactID, SourceNode: t.SourceNode, DestNode: t.DestNode,
		DestPath: t.DestPath, State: t.State, BytesTotal: t.BytesTotal, BytesDone: t.BytesDone,
		CreatedAt: t.CreatedAt,
	}
	if t.Diagnostic.Valid && t.Diagnostic.String != "" {
		d := diag.Diagnostic{Code: "transfer.diagnostic", Severity: "error", Message: t.Diagnostic.String}
		v.Diagnostic = &d
	}
	return v
}

// mapTransferError converts deploy.StartTransfer failures to the
// fragment's stable codes: an invalid source (no valid copy, same node,
// bad dest path) is 422, an offline or unadvertised peer is 503, and an
// unknown node is 404.
func mapTransferError(err error) error {
	msg := err.Error()
	switch {
	case httpx.IsNoRows(err):
		return fmt.Errorf("%w: %s", httpx.ErrNotFound, msg)
	case strings.Contains(msg, "offline"), strings.Contains(msg, "no peer address"):
		return fmt.Errorf("%w: %s", httpx.ErrUnavailable, msg)
	case strings.Contains(msg, "no valid copy"), strings.Contains(msg, "must differ"),
		strings.Contains(msg, "dest_path"), strings.Contains(msg, "no source placement"):
		return fmt.Errorf("%w: %s", httpx.ErrUnprocessable, msg)
	default:
		return err
	}
}

// createTransfer — POST /transfers: plan and start a peer transfer of an
// artifact from an explicit source node (201 with the created transfer).
func (m *Module) createTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if req.ArtifactID == "" || req.SourceNode == "" || req.DestNode == "" || req.DestPath == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
			"artifact_id, source_node, dest_node, and dest_path are required")
		return
	}
	art, err := m.env.Q.GetArtifact(r.Context(), req.ArtifactID)
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.HandleErr(w, httpx.ErrNotFound)
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	tid, err := m.env.Deploy.StartTransfer(r.Context(), art, req.SourceNode, req.DestNode, req.DestPath)
	if err != nil {
		httpx.HandleErr(w, mapTransferError(err))
		return
	}
	t, err := m.env.Q.GetTransfer(r.Context(), tid)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toTransferView(t))
}

// listTransfers — GET /transfers.
func (m *Module) listTransfers(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListTransfers(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]transferView, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTransferView(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getTransfer — GET /transfers/{id}.
func (m *Module) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := m.env.Q.GetTransfer(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.HandleErr(w, httpx.ErrNotFound)
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTransferView(t))
}

// transferTerminal lists the states that cannot be cancelled (the core
// also records "succeeded" once the destination placement validates).
var transferTerminal = map[string]bool{
	"complete": true, "failed": true, "cancelled": true, "succeeded": true,
}

// cancelTransfer — DELETE /transfers/{id}: mark the transfer cancelled;
// the agent's resumable temporary state is preserved for a later retry.
func (m *Module) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := m.env.Q.GetTransfer(r.Context(), id)
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.HandleErr(w, httpx.ErrNotFound)
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if transferTerminal[t.State] {
		httpx.HandleErr(w, fmt.Errorf("%w: transfer is already %s", httpx.ErrConflict, t.State))
		return
	}
	if err := m.env.Q.UpdateTransferState(r.Context(), db.UpdateTransferStateParams{
		State:      "cancelled",
		Diagnostic: sql.NullString{String: "cancelled by operator", Valid: true},
		ID:         id,
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	updated, err := m.env.Q.GetTransfer(r.Context(), id)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTransferView(updated))
}

// manifestSummary parses a stored manifest and returns its high-risk
// permissions and artifact count (false on malformed JSON).
func manifestSummary(manifestJSON string) (risk []string, artifactCount int, ok bool) {
	var m recipe.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		return nil, 0, false
	}
	return m.HighRiskPermissions(), len(m.Artifacts), true
}

// diffLists returns the sorted members of a not present in b.
func diffLists(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, v := range b {
		inB[v] = true
	}
	out := []string{}
	for _, v := range a {
		if !inB[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// semverCmp compares "X.Y.Z" versions (prerelease suffixes ignored);
// missing or non-numeric fields compare as zero.
func semverCmp(a, b string) int {
	na, nb := semverParts(a), semverParts(b)
	for i := range 3 {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func semverParts(v string) [3]int {
	v = strings.SplitN(strings.TrimSpace(v), "-", 2)[0]
	f := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := range 3 {
		if i >= len(f) {
			break
		}
		out[i], _ = strconv.Atoi(strings.TrimRight(f[i], "0123456789"))
	}
	return out
}

func nullStrPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}
