package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

func (s *Service) investigationInventory(ctx context.Context, draft *Draft, request *recipeassistant.Request) error {
	if draft.ResolvedCommit == "" || len(draft.Candidates) == 0 {
		return newError("recipe.draft_source_unavailable", "Inspect the repository before requesting its launch procedures.", false)
	}
	request.Mode = "add"
	for _, candidate := range draft.Candidates {
		if candidate.Origin != OriginSource {
			continue
		}
		_, data, err := s.ReadOwnedFile(ctx, draft.ID, candidate.Path, candidate.SHA256, draft.ResolvedCommit)
		if err != nil {
			return err
		}
		file := recipeassistant.SourceFile{Path: candidate.Path, SHA256: candidate.SHA256, Size: int64(len(data)), Readable: !candidate.Binary && utf8.Valid(data)}
		if !file.Readable {
			file.Reason = "binary or non-UTF-8 source; retained but not sent as text"
		}
		request.SourceInventory = append(request.SourceInventory, file)
	}
	data, err := os.ReadFile(filepath.Join(s.root, draft.ID, "exclusions.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		var exclusions []ContextExclusion
		if err := json.Unmarshal(data, &exclusions); err != nil {
			return err
		}
		for _, excluded := range exclusions {
			request.SourceInventory = append(request.SourceInventory, recipeassistant.SourceFile{Path: excluded.Path, Size: excluded.SizeBytes, Reason: excluded.Reason})
		}
	}
	return nil
}

// Investigation callbacks are installed only after the approved inventory digest
// is checked. Every returned byte is rehashed and recorded for evidence checks.
func (s *Service) attachInvestigation(ctx context.Context, draftID, commit string, request *recipeassistant.Request) {
	var mu sync.Mutex
	record := func(file recipeassistant.ContextFile) {
		mu.Lock()
		defer mu.Unlock()
		request.Context = append(request.Context, file)
	}
	request.ReadSource = func(ctx context.Context, name string) (recipeassistant.ContextFile, error) {
		for _, source := range request.SourceInventory {
			if source.Path != name || !source.Readable {
				continue
			}
			_, data, err := s.ReadOwnedFile(ctx, draftID, name, source.SHA256, commit)
			if err != nil {
				return recipeassistant.ContextFile{}, err
			}
			file := recipeassistant.ContextFile{Path: name, SHA256: source.SHA256, Origin: OriginSource, SourceCommit: commit, Content: string(data)}
			record(file)
			return file, nil
		}
		return recipeassistant.ContextFile{}, newError("recipe.proposal_evidence_invalid", "Requested source is not in the approved readable inventory: "+name, false)
	}
	request.RecordLinkedSource = func(ctx context.Context, file recipeassistant.ContextFile) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		u, err := url.Parse(file.Path)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || file.SourceCommit != "" || !utf8.ValidString(file.Content) || len(file.Content) > maxFileBytes {
			return newError("recipe.proposal_evidence_invalid", "Linked documentation has invalid provenance: "+file.Path, false)
		}
		sum := sha256.Sum256([]byte(file.Content))
		if hex.EncodeToString(sum[:]) != file.SHA256 || !((file.StartLine == 0 && file.EndLine == 0) || (file.StartLine == 1 && file.EndLine == contextLineCount([]byte(file.Content)))) {
			return newError("recipe.proposal_evidence_invalid", "Linked documentation hash or range is invalid", false)
		}
		if err := storeGeneratedBlob(filepath.Join(s.root, draftID, "source", "sha256-"+file.SHA256), []byte(file.Content)); err != nil {
			return err
		}
		record(file)
		return nil
	}
}

func (s *Service) verifyProcedureEvidence(ctx context.Context, draft *Draft, procedure Procedure, candidates []Candidate, allowCompiler bool) error {
	draftID := draft.ID
	request := recipeassistant.Request{Mode: "add", Manifest: draft.Manifest, SourcePins: map[string]any{"commit": draft.ResolvedCommit}}
	if draft.ChangeContext != nil {
		request.Mode = draft.ChangeContext.Kind
	}
	loaded := map[string]bool{}
	for _, evidence := range procedure.Evidence {
		if !strings.HasPrefix(evidence.SourcePath, "https://") {
			if _, err := findCandidate(candidates, evidence.SourcePath, evidence.SHA256, ""); err != nil {
				return err
			}
		}
		if len(evidence.SHA256) != 64 {
			return newError("recipe.proposal_evidence_invalid", "Evidence hash is invalid", false)
		}
		if _, err := hex.DecodeString(evidence.SHA256); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+evidence.SHA256))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != evidence.SHA256 || evidence.StartLine < 1 || evidence.EndLine < evidence.StartLine || evidence.EndLine > contextLineCount(data) {
			return newError("recipe.proposal_evidence_invalid", "Evidence bytes or line range changed: "+evidence.SourcePath, false)
		}
		origin := ""
		if candidate, err := findCandidate(candidates, evidence.SourcePath, evidence.SHA256, OriginSource); err == nil && candidate.Origin == OriginSource && evidence.SourceCommit == draft.ResolvedCommit {
			origin = OriginSource
		}
		request.Context = append(request.Context, recipeassistant.ContextFile{Path: evidence.SourcePath, SHA256: evidence.SHA256, Origin: origin, SourceCommit: evidence.SourceCommit, Content: string(data)})
		loaded[evidence.SourcePath] = true
	}
	// A native proposal may have no individual field citations: its entire
	// authored manifest is the evidence, not a compiler's generated helper.
	for _, candidate := range candidates {
		if candidate.Origin != OriginSource || loaded[candidate.Path] || (filepath.Base(candidate.Path) != "recipe.json" && filepath.Base(candidate.Path) != "recipe.yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+candidate.SHA256))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != candidate.SHA256 {
			return newError("recipe.proposal_evidence_invalid", "Native manifest bytes changed: "+candidate.Path, false)
		}
		request.Context = append(request.Context, recipeassistant.ContextFile{Path: candidate.Path, SHA256: candidate.SHA256, Origin: OriginSource, SourceCommit: draft.ResolvedCommit, Content: string(data)})
	}
	encoded, err := json.Marshal(procedure)
	if err != nil {
		return err
	}
	var result recipeassistant.Result
	if err := json.Unmarshal(encoded, &result); err != nil {
		return err
	}
	if err := validateSourceOwnedProposal(result, request); err != nil {
		if allowCompiler {
			trusted, compileErr := s.trustedCompiledContract(ctx, draft, procedure)
			if compileErr != nil {
				return compileErr
			}
			if trusted {
				return ctx.Err()
			}
		}
		return err
	}
	return ctx.Err()
}
