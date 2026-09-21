package sourceconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MergeLocalChanges preserves local edits relative to original while adopting
// independent incoming edits. Overlapping base-line ranges prefer local; adjacent
// ranges remain independent. Inputs and results are bounded by MaxSourceBytes,
// simultaneous edits require UTF-8 text, and no conflict markers are generated.
// Temporary diff inputs live under the caller-owned directory and are removed.
func MergeLocalChanges(ctx context.Context, directory string, original, local, incoming []byte) ([]byte, error) {
	for _, data := range [][]byte{original, local, incoming} {
		if len(data) > MaxSourceBytes {
			return nil, fmt.Errorf("source.preservation_size_limit")
		}
	}
	if bytes.Equal(original, local) || bytes.Equal(local, incoming) {
		return incoming, nil
	}
	if bytes.Equal(original, incoming) {
		return local, nil
	}
	for _, data := range [][]byte{original, local, incoming} {
		if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			return nil, fmt.Errorf("source.preservation_unsupported_text_merge")
		}
		if bytes.Count(data, []byte{'\n'}) > 65536 {
			return nil, fmt.Errorf("source.preservation_line_limit")
		}
	}
	dir, err := os.MkdirTemp(directory, ".source-merge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for name, data := range map[string][]byte{"original": original, "local": local, "incoming": incoming} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return nil, err
		}
	}
	localEdits, err := sourceTextEdits(ctx, dir, "local", local)
	if err != nil {
		return nil, err
	}
	incomingEdits, err := sourceTextEdits(ctx, dir, "incoming", incoming)
	if err != nil {
		return nil, err
	}
	edits := append([]sourceTextEdit(nil), localEdits...)
	for _, candidate := range incomingEdits {
		overlap := false
		for _, local := range localEdits {
			if local.start > candidate.end {
				break
			}
			if (local.start < candidate.end && candidate.start < local.end) ||
				(local.start == local.end && candidate.start == candidate.end && local.start == candidate.start) ||
				(local.start == local.end && candidate.start < local.start && local.start < candidate.end) ||
				(candidate.start == candidate.end && local.start < candidate.start && candidate.start < local.end) {
				overlap = true
				break
			}
		}
		if !overlap {
			edits = append(edits, candidate)
		}
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end < edits[j].end
		}
		return edits[i].start < edits[j].start
	})
	lines := sourceTextLines(original)
	var result bytes.Buffer
	position := 0
	for _, edit := range edits {
		if edit.start < position || edit.end > len(lines) {
			return nil, fmt.Errorf("source.preservation_diff_invalid")
		}
		for _, line := range lines[position:edit.start] {
			result.WriteString(line)
		}
		for _, line := range edit.lines {
			result.WriteString(line)
		}
		if result.Len() > MaxSourceBytes {
			return nil, fmt.Errorf("source.preservation_merge_too_large")
		}
		position = edit.end
	}
	for _, line := range lines[position:] {
		result.WriteString(line)
	}
	if result.Len() > MaxSourceBytes {
		return nil, fmt.Errorf("source.preservation_merge_too_large")
	}
	return result.Bytes(), nil
}

type sourceTextEdit struct {
	start, end int
	lines      []string
}

func sourceTextLines(data []byte) []string {
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func sourceTextRange(token string) (int, int, error) {
	first, countText, counted := strings.Cut(token[1:], ",")
	start, err := strconv.Atoi(first)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("source.preservation_diff_invalid")
	}
	count := 1
	if counted {
		count, err = strconv.Atoi(countText)
		if err != nil || count < 0 {
			return 0, 0, fmt.Errorf("source.preservation_diff_invalid")
		}
	}
	if count != 0 {
		start--
	}
	if start < 0 {
		return 0, 0, fmt.Errorf("source.preservation_diff_invalid")
	}
	return start, count, nil
}

func sourceTextEdits(ctx context.Context, dir, name string, content []byte) ([]sourceTextEdit, error) {
	lines := sourceTextLines(content)
	if len(lines) > 65536 {
		return nil, fmt.Errorf("source.preservation_line_limit")
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-index", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--diff-algorithm=myers", "--no-indent-heuristic", "--inter-hunk-context=0", "--text", "--unified=0", "--no-prefix", "--", filepath.Join(dir, "original"), filepath.Join(dir, name))
	patch, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return nil, fmt.Errorf("source.preservation_diff: %w", err)
	}
	var edits []sourceTextEdit
	for _, line := range strings.Split(string(patch), "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return nil, fmt.Errorf("source.preservation_diff_invalid")
		}
		start, count, err := sourceTextRange(fields[1])
		if err != nil {
			return nil, err
		}
		next, size, err := sourceTextRange(fields[2])
		if err != nil || next > len(lines) || size > len(lines)-next {
			return nil, fmt.Errorf("source.preservation_diff_invalid")
		}
		if count == size {
			// Equal-size replacements can be resolved one base line at a time.
			for i := range count {
				edits = append(edits, sourceTextEdit{start + i, start + i + 1, lines[next+i : next+i+1]})
			}
		} else {
			edits = append(edits, sourceTextEdit{start, start + count, lines[next : next+size]})
		}
		if len(edits) > 4096 {
			return nil, fmt.Errorf("source.preservation_hunk_limit")
		}
	}
	return edits, nil
}
