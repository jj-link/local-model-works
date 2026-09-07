package recipebuilder

import "regexp"

const MaxRunExcerptBytes = 64 << 10

var runSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s"']+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|secret)\s*[:=]\s*)[^\s,"']+`),
	regexp.MustCompile(`\b(?:sk|ghp|github_pat)_[A-Za-z0-9_-]{16,}\b`),
}

// RedactRunExcerpt removes common credential forms from log text selected for
// assistant context. Redaction happens before the excerpt leaves the server.
func RedactRunExcerpt(input []byte) string {
	redacted := string(input)
	for _, pattern := range runSecretPatterns {
		redacted = pattern.ReplaceAllString(redacted, `${1}[REDACTED]`)
	}
	return redacted
}
