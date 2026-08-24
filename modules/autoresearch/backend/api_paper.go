package backend

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/id"
	runsvc "github.com/jj-link/local-model-works/internal/runs"
)

const maxPaperSourceFile = 5 << 20

var editablePaperExtensions = map[string]bool{
	".tex": true, ".bib": true, ".md": true, ".sty": true, ".cls": true, ".json": true,
}

func digestBytes(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

func quotedETag(digest string) string { return `"` + digest + `"` }

func normalizeETag(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "W/")
	return strings.Trim(value, `"`)
}

func (m *Module) paperRoot(projectID string) string {
	return filepath.Join(m.projectRoot(projectID), "artifacts", "workspace", "project", "paper")
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func securePaperPath(root, requested string, requireExisting bool) (string, error) {
	decoded, err := url.PathUnescape(requested)
	if err != nil || strings.ContainsRune(decoded, '\x00') || filepath.IsAbs(decoded) || filepath.VolumeName(decoded) != "" {
		return "", errors.New("autoresearch.paper_path_invalid")
	}
	clean := filepath.Clean(filepath.FromSlash(decoded))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("autoresearch.paper_path_invalid")
	}
	root = filepath.Clean(root)
	candidate := filepath.Join(root, clean)
	if !pathWithin(root, candidate) {
		return "", errors.New("autoresearch.paper_path_invalid")
	}
	parent := filepath.Dir(candidate)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.New("autoresearch.paper_parent_invalid")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("autoresearch.paper_root_missing")
	}
	if !pathWithin(resolvedRoot, resolvedParent) && resolvedParent != resolvedRoot {
		return "", errors.New("autoresearch.paper_symlink_escape")
	}
	if requireExisting {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || !pathWithin(resolvedRoot, resolved) {
			return "", errors.New("autoresearch.paper_file_missing")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("autoresearch.paper_file_invalid")
		}
		return resolved, nil
	}
	return candidate, nil
}

func paperFileView(root, path string, contents []byte) AutoResearchPaperFile {
	relative, _ := filepath.Rel(root, path)
	return AutoResearchPaperFile{Path: filepath.ToSlash(relative), Sha256: digestBytes(contents), Size: len(contents)}
}

func (m *Module) requireProject(w http.ResponseWriter, r *http.Request, projectID string) (db.AutoresearchProject, bool) {
	project, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return db.AutoresearchProject{}, false
		}
		httpx.HandleErr(w, err)
		return db.AutoresearchProject{}, false
	}
	return project, true
}

func (m *Module) ListAutoResearchPaperFiles(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	if _, ok := m.requireProject(w, r, projectID.String()); !ok {
		return
	}
	root := m.paperRoot(projectID.String())
	files := []AutoResearchPaperFile{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !editablePaperExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, paperFileView(root, path, contents))
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httpx.WriteJSON(w, http.StatusOK, files)
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	httpx.WriteJSON(w, http.StatusOK, files)
}

func (m *Module) GetAutoResearchPaperFile(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId, requested string) {
	if _, ok := m.requireProject(w, r, projectID.String()); !ok {
		return
	}
	path, err := securePaperPath(m.paperRoot(projectID.String()), requested, true)
	if err != nil || !editablePaperExtensions[strings.ToLower(filepath.Ext(path))] {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "autoresearch.paper_path_invalid", "invalid paper source path")
		return
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("ETag", quotedETag(digestBytes(contents)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contents)
}

func (m *Module) UpdateAutoResearchPaperFile(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId, requested string, params UpdateAutoResearchPaperFileParams) {
	if _, ok := m.requireProject(w, r, projectID.String()); !ok {
		return
	}
	root := m.paperRoot(projectID.String())
	path, err := securePaperPath(root, requested, true)
	if err != nil || !editablePaperExtensions[strings.ToLower(filepath.Ext(path))] {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "autoresearch.paper_path_invalid", "invalid paper source path")
		return
	}
	before, err := os.ReadFile(path)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if normalizeETag(params.IfMatch) != digestBytes(before) {
		httpx.WriteErr(w, http.StatusConflict, "paper.edit_conflict", "paper file changed since it was read")
		return
	}
	contents, err := io.ReadAll(io.LimitReader(r.Body, maxPaperSourceFile+1))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if len(contents) > maxPaperSourceFile {
		httpx.WriteErr(w, http.StatusRequestEntityTooLarge, "autoresearch.paper_file_too_large", "paper source exceeds 5 MiB")
		return
	}
	artifacts := filepath.Join(m.projectRoot(projectID.String()), "artifacts")
	if err := requireCleanArtifacts(artifacts); err != nil {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.artifacts_dirty", err.Error())
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".edit-*")
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(contents)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporaryName, path)
	}
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	relative, _ := filepath.Rel(artifacts, path)
	if err := commitArtifacts(artifacts, "paper: edit "+filepath.ToSlash(relative), filepath.ToSlash(relative)); err != nil {
		_ = os.WriteFile(path, before, 0o600)
		httpx.HandleErr(w, err)
		return
	}
	view := paperFileView(root, path, contents)
	w.Header().Set("ETag", quotedETag(view.Sha256))
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (m *Module) GetAutoResearchPaperPdf(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	if _, ok := m.requireProject(w, r, projectID.String()); !ok {
		return
	}
	path := filepath.Join(m.paperRoot(projectID.String()), "build", "manuscript.pdf")
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "compiled paper not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if !strings.HasPrefix(string(contents[:min(len(contents), 5)]), "%PDF-") {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.paper_pdf_invalid", "compiled artifact is not a PDF")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("ETag", quotedETag(digestBytes(contents)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contents)
}

func (m *Module) submitPaperJob(r *http.Request, project db.AutoresearchProject, kind string, input map[string]any) (string, error) {
	input["project_id"] = project.ID
	input["provider_config"] = m.projectConfigWithDefaults(r.Context(), project.ConfigJson)
	runID, err := m.env.Jobs.Submit(r.Context(), kind, input)
	if err != nil {
		return "", err
	}
	if err := m.env.Q.CreateAutoResearchRun(r.Context(), db.CreateAutoResearchRunParams{
		RunID: runID, ProjectID: project.ID, Factory: "paper", WorkerNodeID: project.RunnerNodeID, ConfigSnapshot: project.ConfigJson,
	}); err != nil {
		_ = m.env.Runs.Cancel(r.Context(), runID)
		return "", err
	}
	return runID, nil
}

func (m *Module) CompileAutoResearchPaper(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	project, ok := m.requireProject(w, r, projectID.String())
	if !ok {
		return
	}
	runID, err := m.submitPaperJob(r, project, "autoresearch-paper-compile", map[string]any{})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, err := m.env.Runs.Get(r.Context(), runID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, run)
}

func (m *Module) ChatEditAutoResearchPaper(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	project, ok := m.requireProject(w, r, projectID.String())
	if !ok {
		return
	}
	var req AutoResearchPaperChatRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	for requested, expected := range req.BaseEtags {
		path, err := securePaperPath(m.paperRoot(projectID.String()), requested, true)
		if err != nil {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "autoresearch.paper_path_invalid", requested)
			return
		}
		contents, err := os.ReadFile(path)
		if err != nil || normalizeETag(expected) != digestBytes(contents) {
			httpx.WriteErr(w, http.StatusConflict, "paper.edit_conflict", "paper file changed since chat context was read")
			return
		}
	}
	messageID, err := id.New()
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	emptyChanges, _ := jsonMarshal([]string{})
	if err := m.env.Q.CreateAutoResearchMessage(r.Context(), db.CreateAutoResearchMessageParams{
		ID: messageID, ProjectID: project.ID, Role: "human", Body: req.Message, ChangedPathsJson: string(emptyChanges),
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	runID, err := m.submitPaperJob(r, project, "autoresearch-paper-edit", map[string]any{"message": req.Message, "base_etags": req.BaseEtags})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, err := m.waitForPaperRun(r, runID)
	if err != nil {
		if strings.Contains(err.Error(), "paper.edit_conflict") {
			httpx.WriteErr(w, http.StatusConflict, "paper.edit_conflict", err.Error())
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	changed, _ := run.Output["changed_paths"].([]any)
	if typed, ok := run.Output["changed_paths"].([]string); ok {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"run": run, "changed_paths": typed, "before_digests": run.Output["before_digests"], "after_digests": run.Output["after_digests"],
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"run": run, "changed_paths": changed, "before_digests": run.Output["before_digests"], "after_digests": run.Output["after_digests"],
	})
}

func (m *Module) ReleaseAutoResearchPaper(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	project, ok := m.requireProject(w, r, projectID.String())
	if !ok {
		return
	}
	runID, err := m.submitFactoryRun(r, project, "paper", map[string]any{"release": true}, "")
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, err := m.env.Runs.Get(r.Context(), runID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, run)
}

func (m *Module) waitForPaperRun(r *http.Request, runID string) (runsvc.Run, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := m.env.Runs.Get(r.Context(), runID)
		if err != nil {
			return runsvc.Run{}, err
		}
		if runsvc.State(run.State).Terminal() {
			if run.State != string(runsvc.Succeeded) {
				message := "paper job failed"
				if run.ErrorMessage != nil {
					message = *run.ErrorMessage
				}
				return run, errors.New(message)
			}
			return run, nil
		}
		select {
		case <-r.Context().Done():
			return runsvc.Run{}, r.Context().Err()
		case <-ticker.C:
		}
	}
}

func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}
