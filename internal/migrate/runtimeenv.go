package migrate

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// metaKeys is the exact runtime.env key order the legacy parser enforces
// (dgx_dashboard/control/tools/parse-runtime-env.py).
var metaKeys = []string{
	"IMAGE", "MODEL", "MODEL_REVISION", "MODEL_HOST_PATH", "SERVED",
	"DRAFTER", "DRAFTER_REVISION", "DRAFTER_HOST_PATH",
	"TOKENIZER", "TOKENIZER_REVISION", "TOKENIZER_HOST_PATH", "CONTAINER_NAME",
}

var (
	revRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// segment is one lowercase image-name segment.
	segment       = `[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?`
	imageDigestRe = regexp.MustCompile(
		`^` + segment + `(?::[0-9]+)?(?:/` + segment + `)+@sha256:[0-9a-f]{64}$`)
	localImageRe = regexp.MustCompile(
		`^` + segment + `(?:/` + segment + `)*:[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	servedRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+,-]*$`)
	containerRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	repositoryRe = regexp.MustCompile(
		`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?/[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._/+:-]+$`)
)

// RuntimeMeta is one parsed runtime.env record.
type RuntimeMeta struct {
	Image           string
	Model           string
	ModelRevision   string
	ModelHostPath   string
	Served          string
	Drafter         string
	DrafterRevision string
	DrafterHostPath string
	Tokenizer       string
	TokenizerRev    string
	TokenizerHost   string
	ContainerName   string
}

// ImageDigestPinned reports whether IMAGE carries an @sha256 digest.
func (m RuntimeMeta) ImageDigestPinned() bool {
	return strings.Contains(m.Image, "@sha256:")
}

// ImageDigest returns the pinned digest when pinned, else "".
func (m RuntimeMeta) ImageDigest() string {
	i := strings.Index(m.Image, "@")
	if i < 0 {
		return ""
	}
	return m.Image[i+1:]
}

// ParseRuntimeEnv is the strict Go port of the legacy parse-runtime-env.py:
// exactly 12 assignments in fixed key order, LF endings, no NUL/control
// bytes, and every value regex-checked exactly as the legacy parser does.
func ParseRuntimeEnv(raw []byte) (RuntimeMeta, error) {
	var m RuntimeMeta
	if strings.ContainsRune(string(raw), '\x00') {
		return m, fmt.Errorf("runtime metadata contains a NUL byte")
	}
	text := string(raw)
	if !isUTF8(raw) {
		return m, fmt.Errorf("runtime metadata is not UTF-8")
	}
	if strings.ContainsRune(text, '\r') {
		return m, fmt.Errorf("runtime metadata must use LF line endings")
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != len(metaKeys) {
		return m, fmt.Errorf("runtime metadata must contain exactly %d assignments", len(metaKeys))
	}
	values := make([]string, len(metaKeys))
	for i, expected := range metaKeys {
		key, value, ok := strings.Cut(lines[i], "=")
		if !ok || key != expected {
			return m, fmt.Errorf("expected assignment for %s", expected)
		}
		if err := checkControl(value); err != nil {
			return m, fmt.Errorf("%s: %w", expected, err)
		}
		values[i] = value
	}
	m.Image = values[0]
	m.Model = values[1]
	m.ModelRevision = values[2]
	m.ModelHostPath = values[3]
	m.Served = values[4]
	m.Drafter = values[5]
	m.DrafterRevision = values[6]
	m.DrafterHostPath = values[7]
	m.Tokenizer = values[8]
	m.TokenizerRev = values[9]
	m.TokenizerHost = values[10]
	m.ContainerName = values[11]

	if !imageDigestRe.MatchString(m.Image) {
		if !localImageRe.MatchString(m.Image) || strings.HasSuffix(m.Image, ":latest") {
			return m, fmt.Errorf("IMAGE must be an immutable digest reference or a versioned local tag")
		}
	}
	if !servedRe.MatchString(m.Served) {
		return m, fmt.Errorf("SERVED is invalid")
	}
	if !containerRe.MatchString(m.ContainerName) {
		return m, fmt.Errorf("CONTAINER_NAME is invalid")
	}
	if err := validateArtifact("MODEL", m.Model, m.ModelRevision, m.ModelHostPath, false); err != nil {
		return m, err
	}
	if err := validateArtifact("DRAFTER", m.Drafter, m.DrafterRevision, m.DrafterHostPath, true); err != nil {
		return m, err
	}
	if err := validateArtifact("TOKENIZER", m.Tokenizer, m.TokenizerRev, m.TokenizerHost, true); err != nil {
		return m, err
	}
	return m, nil
}

func checkControl(v string) error {
	for _, c := range v {
		if c < 32 || c == 127 {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func isUTF8(b []byte) bool {
	return strings.ToValidUTF8(string(b), "") == string(b)
}

// canonicalPosixPath mirrors the legacy validate_path checks: safe
// characters, canonical absolute path, no dot segments.
func canonicalPosixPath(value string) bool {
	if !pathRe.MatchString(value) {
		return false
	}
	if path.Clean(value) != value || strings.Contains(value, "//") {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validateArtifact(label, value, revision, hostPath string, optional bool) error {
	if value == "" {
		if optional && revision == "" && hostPath == "" {
			return nil
		}
		return fmt.Errorf("%s must not be empty", label)
	}
	if hostPath != "" {
		if revision != "" {
			return fmt.Errorf("%s_REVISION must be empty when %s_HOST_PATH is set", label, label)
		}
		if !canonicalPosixPath(value) {
			return fmt.Errorf("%s contains unsafe path characters", label)
		}
		if !canonicalPosixPath(hostPath) {
			return fmt.Errorf("%s_HOST_PATH contains unsafe path characters", label)
		}
		return nil
	}
	if !repositoryRe.MatchString(value) {
		return fmt.Errorf("%s must be an owner/repository identifier", label)
	}
	if !revRe.MatchString(revision) {
		return fmt.Errorf("%s_REVISION must be 40 lowercase hexadecimal characters", label)
	}
	return nil
}
