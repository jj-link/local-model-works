package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// ReadOwnedFile returns bytes only through a candidate record owned by the draft.
func (s *Service) ReadOwnedFile(ctx context.Context, draftID, name, hash, sourceCommit string) (Candidate, []byte, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return Candidate{}, nil, err
	}
	if !canonicalDraftPath(name) {
		return Candidate{}, nil, newError("recipe.draft_file_invalid", "file path must be canonical and relative", false)
	}
	draft, err := render(row)
	if err != nil {
		return Candidate{}, nil, err
	}
	inventory, err := contextInventory(draft, sourceCommit)
	if err != nil {
		return Candidate{}, nil, err
	}
	for _, candidate := range inventory {
		if candidate.Path != name || candidate.SHA256 != hash {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+candidate.SHA256))
		if err != nil {
			return Candidate{}, nil, newError("recipe.draft_file_unavailable", "owned file content is unavailable", true)
		}
		actual := sha256.Sum256(body)
		if hex.EncodeToString(actual[:]) != candidate.SHA256 {
			return Candidate{}, nil, newError("recipe.draft_source_changed", "owned file content hash changed", false)
		}
		return candidate, body, nil
	}
	return Candidate{}, nil, newError("recipe.draft_file_unknown", "file is not owned by this draft", false)
}

// UpdateGeneratedFile edits only an accepted generated UTF-8 asset and
// invalidates package/review state under the draft version CAS.
func (s *Service) UpdateGeneratedFile(ctx context.Context, draftID string, version int64, name, content string) (*Draft, error) {
	if !canonicalDraftPath(name) || !utf8.ValidString(content) || len(content) > 256<<10 {
		return nil, newError("recipe.draft_file_invalid", "generated file must be canonical UTF-8 no larger than 256 KiB", false)
	}
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.Version != version {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	if row.Operation.Valid {
		return nil, newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	if row.State == "installed" {
		return nil, newError("recipe.draft_immutable", "installed drafts are immutable", false)
	}
	candidates := renderCandidates(row.Candidates)
	index := -1
	for i := range candidates {
		if candidates[i].Path == name && candidates[i].Origin == OriginGenerated {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, newError("recipe.draft_file_immutable", "only accepted generated files can be edited", false)
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if err := storeGeneratedBlob(filepath.Join(s.root, draftID, "source", "sha256-"+hash), []byte(content)); err != nil {
		return nil, err
	}
	previousHash := candidates[index].SHA256
	candidates[index].SHA256 = hash
	candidates[index].Size = int64(len(content))
	var selected []AssetSelection
	_ = json.Unmarshal([]byte(row.SelectedAssets), &selected)
	for i := range selected {
		if selected[i].Path == name && selected[i].SHA256 == previousHash && (selected[i].Origin == "" || selected[i].Origin == OriginGenerated) {
			selected[i].SHA256 = hash
			selected[i].Origin = OriginGenerated
		}
	}
	manifest := json.RawMessage(row.Manifest)
	findings := append(retainedFindings(row.Diagnostics), s.validatorFindings(manifest)...)
	findings = append(findings, manifestAssetFindings(manifest, candidates, selected)...)
	state := editableState(manifest, renderQuestions(row.Questions), candidates, selected, findings, nil)
	candidatesJSON, _ := marshalJSON(candidates)
	selectedJSON, _ := marshalJSON(selected)
	findingsJSON, _ := marshalJSON(findings)
	result, err := s.db.ExecContext(ctx, `UPDATE recipe_drafts SET state=?, candidates=?, selected_assets=?, diagnostics=?,
package_digest=NULL, acknowledged_warnings='[]', resolved_references='[]', version=version+1,
updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND version=? AND operation IS NULL AND state!='installed'`,
		state, candidatesJSON, selectedJSON, findingsJSON, draftID, version)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	return s.Get(ctx, draftID)
}

func canonicalDraftPath(value string) bool {
	return value != "" && value == path.Clean(value) && value != "." && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../") && !strings.Contains(value, `\`)
}

func renderQuestions(encoded string) []Question {
	var questions []Question
	_ = json.Unmarshal([]byte(encoded), &questions)
	return questions
}

// Retain source-safety findings and other still-applicable non-validation
// findings, but not a dismissed/retried operation's terminal error.
func retainedFindings(encoded string) []Diagnostic {
	var findings []Diagnostic
	_ = json.Unmarshal([]byte(encoded), &findings)
	kept := findings[:0]
	for _, finding := range findings {
		if finding.Phase != PhaseValidate && !(finding.Dismissible && finding.Severity == "error") {
			kept = append(kept, finding)
		}
	}
	return kept
}

func findCandidate(candidates []Candidate, name, hash, origin string) (Candidate, error) {
	var match *Candidate
	foundPath := false
	for i := range candidates {
		c := &candidates[i]
		if c.Path != name {
			continue
		}
		foundPath = true
		if c.SHA256 != hash || (origin != "" && c.Origin != origin) {
			continue
		}
		if match != nil && match.Origin != c.Origin {
			return Candidate{}, newError("recipe.draft_asset_ambiguous", "select the file origin as well as its path and hash", false)
		}
		match = c
	}
	if match != nil {
		return *match, nil
	}
	if !foundPath {
		return Candidate{}, newError("recipe.draft_asset_unknown", "file is not an inventoried candidate: "+name, false)
	}
	return Candidate{}, newError("recipe.draft_asset_mismatch", "file hash or origin does not match the inventory: "+name, false)
}

func contextInventory(draft *Draft, commit string) ([]Candidate, error) {
	if commit == "" || commit == draft.ResolvedCommit {
		return draft.Candidates, nil
	}
	if change := draft.ChangeContext; change != nil && commit == change.BaseCommit && change.BaseSourceStatus == "available" {
		return change.BaseCandidates, nil
	}
	return nil, newError("recipe.draft_context_unknown", "source commit is not retained by this draft", false)
}

func contextIdentity(file ContextFile) string {
	return file.SourceCommit + "\x00" + file.Path + "\x00" + file.SHA256 + "\x00" + file.Origin
}

func proposalAssets(inventory []Candidate, proposal Proposal) ([]Candidate, []AssetSelection) {
	generated := make(map[string]bool, len(proposal.Files))
	for _, file := range proposal.Files {
		generated[file.Path] = true
	}
	candidates := make([]Candidate, 0, len(inventory)+len(proposal.Files))
	for _, candidate := range inventory {
		if candidate.Origin != OriginGenerated || !generated[candidate.Path] {
			candidates = append(candidates, candidate)
		}
	}
	selected := append([]AssetSelection(nil), proposal.SelectedSourceAssets...)
	for _, file := range proposal.Files {
		sum := sha256.Sum256([]byte(file.Content))
		hash := hex.EncodeToString(sum[:])
		candidates = append(candidates, Candidate{Path: file.Path, Size: int64(len(file.Content)), SHA256: hash, Origin: OriginGenerated, SourcePath: file.SourcePath})
		selected = append(selected, AssetSelection{Path: file.Path, SHA256: hash, Origin: OriginGenerated})
	}
	return candidates, selected
}

func pinnedManifest(row db.RecipeDraft, manifest json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &object); err != nil || object == nil {
		return nil, newError("recipe.draft_manifest_invalid", "manifest must be a JSON object", false)
	}
	var source GitSource
	if err := json.Unmarshal([]byte(row.Source), &source); err != nil {
		return nil, err
	}
	if source.Remote == "" || !row.ResolvedCommit.Valid {
		return manifest, nil
	}
	var metadata map[string]json.RawMessage
	if raw, ok := object["metadata"]; ok {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, newError("recipe.draft_manifest_invalid", "metadata must be an object", false)
		}
	}
	if metadata == nil {
		metadata = map[string]json.RawMessage{}
	}
	if source.Path == "" {
		source.Path = "."
	}
	metadata["source"], _ = json.Marshal(recipe.Source{URL: source.Remote, Path: source.Path, Revision: row.ResolvedCommit.String})
	object["metadata"], _ = json.Marshal(metadata)
	return json.Marshal(object)
}

func storeGeneratedBlob(path string, body []byte) error {
	existing, err := os.ReadFile(path)
	if err == nil {
		if string(existing) != string(body) {
			return newError("recipe.draft_source_changed", "stored content does not match its hash", false)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return newError("recipe.draft_io", "cannot read generated asset store", true)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if os.IsExist(err) {
		existing, err = os.ReadFile(path)
		if err == nil && string(existing) == string(body) {
			return nil
		}
		return newError("recipe.draft_source_changed", "stored content does not match its hash", false)
	}
	if err != nil {
		return newError("recipe.draft_io", "cannot create generated asset", true)
	}
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return newError("recipe.draft_io", "cannot store generated asset", true)
	}
	return nil
}
