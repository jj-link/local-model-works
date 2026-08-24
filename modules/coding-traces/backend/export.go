package backend

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pkoukk/tiktoken-go"

	"github.com/jj-link/local-model-works/internal/cjson"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/traces"
)

type exportSelection struct {
	TraceIDs          []string `json:"trace_ids,omitempty"`
	ExperimentID      string   `json:"experiment_id,omitempty"`
	State             string   `json:"state,omitempty"`
	Model             string   `json:"model,omitempty"`
	TaskIDs           []string `json:"task_ids,omitempty"`
	Tokenizer         string   `json:"tokenizer,omitempty"`
	MaxContextTokens  int      `json:"max_context_tokens,omitempty"`
	SuccessCapPerTask int      `json:"success_cap_per_task,omitempty"`
	Seed              int64    `json:"seed,omitempty"`
}

type exclusion struct {
	TraceID string `json:"trace_id"`
	Dataset string `json:"dataset"`
	Reason  string `json:"reason"`
}
type policyCandidate struct {
	Trace  db.CodingTrace
	Tokens int
}

func (m *Module) runTokenize(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var selection exportSelection
	if err := remarshal(c.Input, &selection); err != nil {
		return nil, err
	}
	normalizeSelection(&selection)
	encoding, err := tiktoken.GetEncoding(selection.Tokenizer)
	if err != nil {
		return nil, fmt.Errorf("tokenizer %s: %w", selection.Tokenizer, err)
	}
	rows, err := m.env.Q.ListCodingTracesForExport(ctx, nullableArg(selection.ExperimentID))
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, row := range filterTraceRows(rows, selection) {
		messages, err := m.policyMessages(ctx, row)
		if err != nil {
			return nil, err
		}
		data, _ := cjson.Marshal(messages)
		counts[row.ID] = len(encoding.Encode(string(data), nil, nil))
	}
	return map[string]any{"tokenizer": selection.Tokenizer, "counts": counts}, nil
}

func (m *Module) runRetention(ctx context.Context, _ *jobs.Context) (map[string]any, error) {
	settingsValue, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return nil, err
	}
	days := number(settingsValue["retention_days"], 0)
	if days == 0 {
		return map[string]any{"deleted": 0, "retention_days": 0}, nil
	}
	deleted, err := m.env.Traces.DeleteExpired(ctx, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": deleted, "retention_days": days}, nil
}

func (m *Module) runExport(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	exportID, _ := c.Input["export_id"].(string)
	var selection exportSelection
	if err := remarshal(c.Input["selection"], &selection); err != nil {
		return nil, err
	}
	normalizeSelection(&selection)
	selectionJSON, _ := cjson.Marshal(selection)
	if err := m.env.Q.CreateCodingTraceExport(ctx, db.CreateCodingTraceExportParams{ID: exportID, RunID: c.RunID, Selection: string(selectionJSON), Seed: selection.Seed}); err != nil {
		existing, getErr := m.env.Q.GetCodingTraceExport(ctx, exportID)
		if getErr != nil || existing.RunID != c.RunID {
			return nil, err
		}
	}
	_, _ = m.env.Q.UpdateCodingTraceExport(ctx, db.UpdateCodingTraceExportParams{State: "running", ID: exportID})
	fail := func(err error) (map[string]any, error) {
		_, _ = m.env.Q.FinishCodingTraceExport(context.WithoutCancel(ctx), db.FinishCodingTraceExportParams{State: "failed", ID: exportID})
		return nil, err
	}
	encoding, err := tiktoken.GetEncoding(selection.Tokenizer)
	if err != nil {
		return fail(fmt.Errorf("tokenizer %s: %w", selection.Tokenizer, err))
	}
	rows, err := m.env.Q.ListCodingTracesForExport(ctx, nullableArg(selection.ExperimentID))
	if err != nil {
		return fail(err)
	}
	rows = filterTraceRows(rows, selection)
	if len(rows) == 0 {
		return fail(fmt.Errorf("export selects no traces"))
	}
	work := filepath.Join(c.Workspace, "dataset")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return fail(err)
	}
	canonicalFile, err := os.OpenFile(filepath.Join(work, "canonical.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	canonical := bufio.NewWriter(canonicalFile)
	var candidates []policyCandidate
	var successes, failures []db.CodingTrace
	var excluded []exclusion
	for _, row := range rows {
		record, err := m.canonicalRecord(ctx, row)
		if err != nil {
			canonicalFile.Close()
			return fail(err)
		}
		if err := writeJSONLine(canonical, record); err != nil {
			canonicalFile.Close()
			return fail(err)
		}
		if row.State != "completed" || !row.SuccessLabel.Valid {
			excluded = append(excluded, exclusion{row.ID, "policy_sft", "not_executable_label"}, exclusion{row.ID, "verifier", "not_executable_label"})
			continue
		}
		if row.SuccessLabel.Int64 == 1 {
			messages, err := m.policyMessages(ctx, row)
			if err != nil {
				canonicalFile.Close()
				return fail(err)
			}
			data, _ := cjson.Marshal(messages)
			tokens := len(encoding.Encode(string(data), nil, nil))
			if tokens >= selection.MaxContextTokens {
				excluded = append(excluded, exclusion{row.ID, "policy_sft", "context_limit"})
			} else {
				candidates = append(candidates, policyCandidate{row, tokens})
			}
			successes = append(successes, row)
		} else {
			excluded = append(excluded, exclusion{row.ID, "policy_sft", "unresolved"})
			failures = append(failures, row)
		}
	}
	if err := canonical.Flush(); err != nil {
		canonicalFile.Close()
		return fail(err)
	}
	if err := canonicalFile.Close(); err != nil {
		return fail(err)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Trace.TaskID != candidates[j].Trace.TaskID {
			return candidates[i].Trace.TaskID < candidates[j].Trace.TaskID
		}
		if candidates[i].Trace.TurnCount != candidates[j].Trace.TurnCount {
			return candidates[i].Trace.TurnCount < candidates[j].Trace.TurnCount
		}
		return candidates[i].Trace.ID < candidates[j].Trace.ID
	})
	selectedPolicy := map[string]policyCandidate{}
	perTask := map[string]int{}
	for _, candidate := range candidates {
		if perTask[candidate.Trace.TaskID] >= selection.SuccessCapPerTask {
			excluded = append(excluded, exclusion{candidate.Trace.ID, "policy_sft", "success_cap"})
			continue
		}
		selectedPolicy[candidate.Trace.ID] = candidate
		perTask[candidate.Trace.TaskID]++
	}
	policyFile, err := os.OpenFile(filepath.Join(work, "policy_sft.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	policy := bufio.NewWriter(policyFile)
	policyIDs := make([]string, 0, len(selectedPolicy))
	for id := range selectedPolicy {
		policyIDs = append(policyIDs, id)
	}
	sort.Strings(policyIDs)
	byID := map[string]db.CodingTrace{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, traceID := range policyIDs {
		row := byID[traceID]
		messages, err := m.policyMessages(ctx, row)
		if err != nil {
			policyFile.Close()
			return fail(err)
		}
		candidate := selectedPolicy[traceID]
		if err := writeJSONLine(policy, map[string]any{"messages": messages, "metadata": map[string]any{"trace_id": row.ID, "task_id": row.TaskID, "tokens": candidate.Tokens, "tokenizer": selection.Tokenizer, "assistant_only": true}}); err != nil {
			policyFile.Close()
			return fail(err)
		}
	}
	if err := policy.Flush(); err != nil {
		policyFile.Close()
		return fail(err)
	}
	if err := policyFile.Close(); err != nil {
		return fail(err)
	}
	verifierIDs := balancedVerifierIDs(successes, failures, selection.Seed)
	verifierSet := stringSet(verifierIDs)
	for _, row := range append(append([]db.CodingTrace(nil), successes...), failures...) {
		if !verifierSet[row.ID] {
			excluded = append(excluded, exclusion{row.ID, "verifier", "class_balance"})
		}
	}
	verifierFile, err := os.OpenFile(filepath.Join(work, "verifier.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	verifier := bufio.NewWriter(verifierFile)
	sort.Strings(verifierIDs)
	for _, traceID := range verifierIDs {
		row := byID[traceID]
		events, err := m.env.Traces.AllEvents(ctx, row.ID)
		if err != nil {
			verifierFile.Close()
			return fail(err)
		}
		judgement := "NO"
		if row.SuccessLabel.Int64 == 1 {
			judgement = "YES"
		}
		if err := writeJSONLine(verifier, map[string]any{"trace_id": row.ID, "task_id": row.TaskID, "problem": row.Problem, "interaction": eventViews(events), "final_diff": nullable(row.FinalDiff), "judgement": "<judgement>" + judgement + "</judgement>"}); err != nil {
			verifierFile.Close()
			return fail(err)
		}
	}
	if err := verifier.Flush(); err != nil {
		verifierFile.Close()
		return fail(err)
	}
	if err := verifierFile.Close(); err != nil {
		return fail(err)
	}
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].TraceID != excluded[j].TraceID {
			return excluded[i].TraceID < excluded[j].TraceID
		}
		if excluded[i].Dataset != excluded[j].Dataset {
			return excluded[i].Dataset < excluded[j].Dataset
		}
		return excluded[i].Reason < excluded[j].Reason
	})
	excludedFile, err := os.OpenFile(filepath.Join(work, "excluded.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	excludedWriter := bufio.NewWriter(excludedFile)
	for _, item := range excluded {
		if err := writeJSONLine(excludedWriter, item); err != nil {
			excludedFile.Close()
			return fail(err)
		}
	}
	if err := excludedWriter.Flush(); err != nil {
		excludedFile.Close()
		return fail(err)
	}
	if err := excludedFile.Close(); err != nil {
		return fail(err)
	}
	manifest := map[string]any{"schema": "localmodelworks/coding-trace-dataset/v1", "trace_schema": traces.SchemaVersion, "redaction_version": traces.RedactionVersion, "transform_version": "swe-gym-paper-transform-v1", "tokenizer": selection.Tokenizer, "max_context_tokens": selection.MaxContextTokens, "success_cap_per_task": selection.SuccessCapPerTask, "seed": selection.Seed, "source_trace_ids": traceIDs(rows), "source_trace_digests": traceDigests(rows), "filters": selection, "counts": map[string]int{"canonical": len(rows), "policy_sft": len(policyIDs), "verifier": len(verifierIDs), "excluded": len(excluded)}}
	manifestData, _ := cjson.Marshal(manifest)
	manifestDigest := digest(manifestData)
	manifest["manifest_digest"] = manifestDigest
	manifestData, _ = cjson.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(work, "manifest.json"), append(manifestData, '\n'), 0o600); err != nil {
		return fail(err)
	}
	archiveName := exportID + ".tar.gz"
	archivePath := filepath.Join(c.Workspace, archiveName)
	if err := writeDeterministicArchive(archivePath, work, []string{"canonical.jsonl", "excluded.jsonl", "manifest.json", "policy_sft.jsonl", "verifier.jsonl"}); err != nil {
		return fail(err)
	}
	artifact, err := c.PublishArtifact("coding-trace-dataset", archiveName)
	if err != nil {
		return fail(err)
	}
	_, err = m.env.Q.FinishCodingTraceExport(ctx, db.FinishCodingTraceExportParams{State: "completed", ArtifactPath: sql.NullString{String: artifact.Path, Valid: true}, ManifestDigest: sql.NullString{String: manifestDigest, Valid: true}, CanonicalCount: int64(len(rows)), PolicyCount: int64(len(policyIDs)), VerifierCount: int64(len(verifierIDs)), ExcludedCount: int64(len(excluded)), ID: exportID})
	if err != nil {
		return fail(err)
	}
	return map[string]any{"export_id": exportID, "artifact": artifact, "manifest_digest": manifestDigest, "canonical_count": len(rows), "policy_count": len(policyIDs), "verifier_count": len(verifierIDs), "excluded_count": len(excluded)}, nil
}

func normalizeSelection(selection *exportSelection) {
	if selection.Tokenizer == "" {
		selection.Tokenizer = "cl100k_base"
	}
	if selection.MaxContextTokens == 0 {
		selection.MaxContextTokens = 32768
	}
	if selection.SuccessCapPerTask == 0 {
		selection.SuccessCapPerTask = 2
	}
}
func filterTraceRows(rows []db.CodingTrace, selection exportSelection) []db.CodingTrace {
	ids := stringSet(selection.TraceIDs)
	tasks := stringSet(selection.TaskIDs)
	out := make([]db.CodingTrace, 0, len(rows))
	for _, row := range rows {
		if len(ids) > 0 && !ids[row.ID] {
			continue
		}
		if len(tasks) > 0 && !tasks[row.TaskID] {
			continue
		}
		if selection.State != "" && row.State != selection.State {
			continue
		}
		if selection.Model != "" && row.Model != selection.Model {
			continue
		}
		out = append(out, row)
	}
	return out
}
func (m *Module) canonicalRecord(ctx context.Context, row db.CodingTrace) (map[string]any, error) {
	events, err := m.env.Traces.AllEvents(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	record := map[string]any{"trace": traceView(row), "events": eventViews(events)}
	if verification, err := m.env.Q.GetCodingTraceVerification(ctx, row.ID); err == nil {
		record["verification"] = verificationView(verification)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return record, nil
}
func eventViews(events []db.CodingTraceEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		out = append(out, eventView(event))
	}
	return out
}
func (m *Module) policyMessages(ctx context.Context, row db.CodingTrace) ([]map[string]any, error) {
	events, err := m.env.Traces.AllEvents(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{{"role": "user", "content": row.Problem}}
	for _, event := range events {
		payload := jsonObject(event.Payload)
		contentBytes, _ := cjson.Marshal(payload)
		content := string(contentBytes)
		role := ""
		switch event.Kind {
		case "message":
			role = "user"
			if object, ok := payload.(map[string]any); ok {
				if value, ok := object["role"].(string); ok {
					role = value
				} else if source, ok := object["source"].(string); ok && source == "agent" {
					role = "assistant"
				}
				if value, ok := object["content"].(string); ok {
					content = value
				}
			}
		case "tool.call":
			role = "assistant"
		case "tool.result":
			role = "tool"
		}
		if role == "" {
			continue
		}
		message := map[string]any{"role": role, "content": content}
		if role == "assistant" {
			message["weight"] = 1
		}
		messages = append(messages, message)
	}
	return messages, nil
}
func balancedVerifierIDs(successes, failures []db.CodingTrace, seed int64) []string {
	successIDs := traceIDs(successes)
	failureIDs := traceIDs(failures)
	sort.Strings(successIDs)
	sort.Strings(failureIDs)
	seedValue := uint64(seed)
	rng := rand.New(rand.NewPCG(seedValue, seedValue^0x9e3779b97f4a7c15))
	rng.Shuffle(len(successIDs), func(i, j int) { successIDs[i], successIDs[j] = successIDs[j], successIDs[i] })
	rng.Shuffle(len(failureIDs), func(i, j int) { failureIDs[i], failureIDs[j] = failureIDs[j], failureIDs[i] })
	count := len(successIDs)
	if len(failureIDs) < count {
		count = len(failureIDs)
	}
	return append(successIDs[:count], failureIDs[:count]...)
}
func traceIDs(rows []db.CodingTrace) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}
func traceDigests(rows []db.CodingTrace) map[string]string {
	out := map[string]string{}
	for _, row := range rows {
		if row.Digest.Valid {
			out[row.ID] = row.Digest.String
		}
	}
	return out
}
func nullableArg(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func writeJSONLine(writer *bufio.Writer, value any) error {
	data, err := cjson.Marshal(value)
	if err != nil {
		return err
	}
	if _, err = writer.Write(data); err != nil {
		return err
	}
	return writer.WriteByte('\n')
}
func writeDeterministicArchive(path, root string, names []string) error {
	sort.Strings(names)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		file.Close()
		return err
	}
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range names {
		source, err := os.Open(filepath.Join(root, name))
		if err != nil {
			return closeArchive(file, gzipWriter, tarWriter, err)
		}
		info, err := source.Stat()
		if err != nil {
			source.Close()
			return closeArchive(file, gzipWriter, tarWriter, err)
		}
		header := &tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}
		if err := tarWriter.WriteHeader(header); err != nil {
			source.Close()
			return closeArchive(file, gzipWriter, tarWriter, err)
		}
		if _, err := io.Copy(tarWriter, source); err != nil {
			source.Close()
			return closeArchive(file, gzipWriter, tarWriter, err)
		}
		source.Close()
	}
	return closeArchive(file, gzipWriter, tarWriter, nil)
}
func closeArchive(file *os.File, gzipWriter *gzip.Writer, tarWriter *tar.Writer, prior error) error {
	if err := tarWriter.Close(); prior == nil {
		prior = err
	}
	if err := gzipWriter.Close(); prior == nil {
		prior = err
	}
	if err := file.Close(); prior == nil {
		prior = err
	}
	return prior
}
