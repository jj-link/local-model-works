package backend

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
)

var candidateHeadings = []string{
	"## Research question",
	"## Motivation",
	"## Proposed mechanism",
	"## Supporting sources",
	"## Falsifiable claims",
	"## Risks and disconfirming evidence",
}

type intakeCandidate struct {
	Title string
	Body  string
}

func parseIntakeCandidate(contents []byte) (intakeCandidate, error) {
	lines := strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") || strings.TrimSpace(strings.TrimPrefix(lines[0], "# ")) == "" {
		return intakeCandidate{}, errors.New("autoresearch.intake_candidate_invalid: missing title")
	}
	position := 1
	for _, heading := range candidateHeadings {
		found := -1
		for index := position; index < len(lines); index++ {
			if lines[index] == heading {
				found = index
				break
			}
			if strings.HasPrefix(lines[index], "## ") {
				break
			}
		}
		if found < 0 {
			return intakeCandidate{}, fmt.Errorf("autoresearch.intake_candidate_invalid: missing or out-of-order %s", heading)
		}
		position = found + 1
	}
	return intakeCandidate{
		Title: strings.TrimSpace(strings.TrimPrefix(lines[0], "# ")),
		Body:  strings.TrimSpace(strings.Join(lines[1:], "\n")),
	}, nil
}

func candidateCount(input map[string]any) (int, bool) {
	value, exists := input["candidate_count"]
	if !exists {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (m *Module) importGeneratedCandidates(ctx context.Context, project db.AutoresearchProject, expected int) ([]string, error) {
	intakeRoot := filepath.Join(m.projectRoot(project.ID), "artifacts", ".lmw", "intake")
	paths, err := filepath.Glob(filepath.Join(intakeRoot, "candidate-*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) != expected {
		return nil, fmt.Errorf("autoresearch.intake_candidate_count: expected %d, found %d", expected, len(paths))
	}
	candidates := make([]intakeCandidate, 0, len(paths))
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		candidate, err := parseIntakeCandidate(contents)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		candidates = append(candidates, candidate)
	}

	tx, err := m.env.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM autoresearch_ideas WHERE project_id=? AND source='generated' AND selected=0`, project.ID); err != nil {
		return nil, err
	}
	for index, candidate := range candidates {
		ideaID, err := id.New()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO autoresearch_ideas
			(id, project_id, ordinal, source, title, body, selected) VALUES (?, ?, ?, 'generated', ?, ?, 0)`,
			ideaID, project.ID, index+1, candidate.Title, candidate.Body); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE autoresearch_projects SET status='awaiting_idea_selection',
		version=version+1, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, project.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	artifacts := filepath.Join(m.projectRoot(project.ID), "artifacts")
	status, err := runGit(artifacts, "status", "--porcelain", "--", ".lmw/intake")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(status)) != "" {
		if err := commitArtifacts(artifacts, "idea: generate intake candidates", ".lmw/intake"); err != nil {
			return nil, err
		}
	}
	changed := make([]string, 0, len(paths)+1)
	for _, path := range paths {
		relative, _ := filepath.Rel(artifacts, path)
		changed = append(changed, filepath.ToSlash(relative))
	}
	if _, err := os.Stat(filepath.Join(intakeRoot, "manifest.json")); err == nil {
		changed = append(changed, ".lmw/intake/manifest.json")
	}
	return changed, nil
}

func projectHumanGate(project db.AutoresearchProject, name string) bool {
	defaults := map[string]bool{"idea_selection": true, "paper_post_edit": true}
	value := defaults[name]
	var config struct {
		HumanGates map[string]bool `json:"human_gates"`
	}
	if json.Unmarshal([]byte(project.ConfigJson), &config) == nil {
		if configured, ok := config.HumanGates[name]; ok {
			value = configured
		}
	}
	return value
}

func (m *Module) publishLifecycleDecision(runID, projectID, gate, message string) {
	payload := map[string]any{
		"schema": 1, "event_id": runID + ":decision:" + gate, "run_id": runID,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": "decision.required",
		"payload": map[string]any{"project_id": projectID, "gate": gate, "message": message},
	}
	encoded, _ := json.Marshal(payload)
	encoded = append(encoded, '\n')
	_ = m.env.Runs.AppendLog(runID, "", 0, "stdout", encoded)
	if info, err := os.Stat(m.env.Runs.LogPath(runID, "", 0, "stdout")); err == nil {
		m.env.Runs.MarkLogEnd(runID, "", 0, "stdout", uint64(info.Size()))
	}
	if m.env.Bus != nil {
		body, _ := json.Marshal(payload["payload"])
		_ = m.env.Bus.Publish(context.Background(), "autoresearch.decision.required", projectID, body)
	}
}

func automaticChildInput(input map[string]any) map[string]any {
	child := map[string]any{}
	for _, key := range []string{"provider_overrides", "ssh_secret_name", "release"} {
		if value, ok := input[key]; ok {
			child[key] = value
		}
	}
	return child
}

func (m *Module) submitLifecycleFactory(ctx context.Context, project db.AutoresearchProject, parent *jobs.Context, factory string, input map[string]any) error {
	_, err := m.submitFactoryRunContext(ctx, project, factory, input, parent.RunID)
	return err
}

func paperPhase(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	frontmatter := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			if frontmatter {
				break
			}
			frontmatter = true
			continue
		}
		if frontmatter && strings.HasPrefix(line, "phase:") {
			phase := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "phase:")), `"'`)
			if phase != "" {
				return phase, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("autoresearch.paper_state_invalid: phase missing")
}

func latestExperimentRequest(paperRoot string) (string, string, error) {
	paths, err := filepath.Glob(filepath.Join(paperRoot, "experiment-requests", "*.md"))
	if err != nil {
		return "", "", err
	}
	if len(paths) == 0 {
		return "", "", errors.New("autoresearch.paper_experiment_request_missing")
	}
	sort.Slice(paths, func(i, j int) bool {
		left, leftErr := os.Stat(paths[i])
		right, rightErr := os.Stat(paths[j])
		if leftErr == nil && rightErr == nil && !left.ModTime().Equal(right.ModTime()) {
			return left.ModTime().After(right.ModTime())
		}
		return paths[i] > paths[j]
	})
	contents, err := os.ReadFile(paths[0])
	if err != nil {
		return "", "", err
	}
	return filepath.ToSlash(filepath.Join("workspace", "project", "paper", "experiment-requests", filepath.Base(paths[0]))), string(contents), nil
}

func (m *Module) setProjectStatus(ctx context.Context, projectID, status string) error {
	rows, err := m.env.Q.UpdateAutoResearchProjectStatus(ctx, db.UpdateAutoResearchProjectStatusParams{Status: status, ID: projectID})
	if err != nil {
		return err
	}
	if rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Module) continueFactoryLifecycle(ctx context.Context, job *jobs.Context, project db.AutoresearchProject, factory string) error {
	input := automaticChildInput(job.Input)
	switch factory {
	case "idea":
		return m.submitLifecycleFactory(ctx, project, job, "proposal", input)
	case "proposal":
		if projectHumanGate(project, "claim_scope_change") {
			m.publishLifecycleDecision(job.RunID, project.ID, "claim_scope_change", "Review the proposed claim scope before literature analysis.")
			return nil
		}
		return m.submitLifecycleFactory(ctx, project, job, "deep_lit", input)
	case "deep_lit":
		if projectHumanGate(project, "citation_change") {
			m.publishLifecycleDecision(job.RunID, project.ID, "citation_change", "Review literature and citation changes before experiments.")
			return nil
		}
		return m.submitLifecycleFactory(ctx, project, job, "experiment", input)
	case "experiment":
		return m.submitLifecycleFactory(ctx, project, job, "paper", input)
	case "paper":
		statePath := filepath.Join(m.paperRoot(project.ID), "PAPER_STATE.md")
		phase, err := paperPhase(statePath)
		if err != nil {
			return err
		}
		switch phase {
		case "needs_experiment":
			requestPath, request, err := latestExperimentRequest(m.paperRoot(project.ID))
			if err != nil {
				return err
			}
			if projectHumanGate(project, "experiment_handback") {
				m.publishLifecycleDecision(job.RunID, project.ID, "experiment_handback", "Paper evidence requires the experiment request at "+requestPath)
				return nil
			}
			input["paper_request"] = request
			return m.submitLifecycleFactory(ctx, project, job, "experiment", input)
		case "awaiting_human_edit":
			if err := m.setProjectStatus(ctx, project.ID, "paper_editing"); err != nil {
				return err
			}
			m.publishLifecycleDecision(job.RunID, project.ID, "paper_post_edit", "The paper is ready for human post-editing and explicit release.")
			return nil
		case "done":
			return m.setProjectStatus(ctx, project.ID, "completed")
		case "failed":
			return m.setProjectStatus(ctx, project.ID, "failed")
		default:
			return m.submitLifecycleFactory(ctx, project, job, "paper", input)
		}
	default:
		return fmt.Errorf("autoresearch.factory_invalid: %s", factory)
	}
}

func (m *Module) runFactoryLifecycle(ctx context.Context, job *jobs.Context, factory string) (map[string]any, error) {
	output, err := m.executeWorker(ctx, job, factory)
	if err != nil {
		return nil, err
	}
	projectID, _ := job.Input["project_id"].(string)
	project, err := m.env.Q.GetAutoResearchProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if factory == "idea" {
		if count, generated := candidateCount(job.Input); generated {
			changed, err := m.importGeneratedCandidates(ctx, project, count)
			if err != nil {
				return nil, err
			}
			output["changed_paths"] = changed
			return output, nil
		}
		selected, err := m.env.Q.CountSelectedAutoResearchIdeas(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		if selected == 0 {
			return nil, errors.New("autoresearch.idea_selection_required")
		}
	}
	if err := m.continueFactoryLifecycle(ctx, job, project, factory); err != nil {
		return nil, err
	}
	return output, nil
}
