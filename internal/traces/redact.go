package traces

import (
	"regexp"
	"sort"
	"strings"
)

const RedactionVersion = "lmw-redaction-v1"

var (
	credentialKey = regexp.MustCompile(`(?i)(^|[_-])(authorization|cookie|password|passwd|secret|token|api[_-]?key|credential|credentials|private[_-]?key)$`)
	bearerToken   = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	knownToken    = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,}|hf_[A-Za-z0-9]{20,})\b`)
	privateKey    = regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----.*?-----END (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)
)

// Redactor removes credentials before any trace payload reaches persistent storage.
type Redactor struct {
	secrets []string
}

func NewRedactor(values ...string) Redactor {
	secrets := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) >= 4 {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return Redactor{secrets: secrets}
}

func (r Redactor) WithValues(values ...string) Redactor {
	all := append(append([]string(nil), r.secrets...), values...)
	return NewRedactor(all...)
}

func (r Redactor) Redact(value any) (any, int) {
	return r.redact(value, "")
}

func (r Redactor) redact(value any, key string) (any, int) {
	if credentialKey.MatchString(key) && value != nil {
		return "[REDACTED:credential]", 1
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		count := 0
		for childKey, child := range typed {
			clean, n := r.redact(child, childKey)
			out[childKey] = clean
			count += n
		}
		return out, count
	case []any:
		out := make([]any, len(typed))
		count := 0
		for i, child := range typed {
			clean, n := r.redact(child, key)
			out[i] = clean
			count += n
		}
		return out, count
	case string:
		return r.redactString(typed)
	default:
		return value, 0
	}
}

func (r Redactor) redactString(value string) (string, int) {
	count := 0
	for _, secret := range r.secrets {
		occurrences := strings.Count(value, secret)
		if occurrences > 0 {
			value = strings.ReplaceAll(value, secret, "[REDACTED:stored-secret]")
			count += occurrences
		}
	}
	for _, rule := range []struct {
		re          *regexp.Regexp
		replacement string
	}{
		{privateKey, "[REDACTED:private-key]"},
		{bearerToken, "[REDACTED:authorization]"},
		{knownToken, "[REDACTED:token]"},
	} {
		matches := rule.re.FindAllStringIndex(value, -1)
		if len(matches) > 0 {
			value = rule.re.ReplaceAllString(value, rule.replacement)
			count += len(matches)
		}
	}
	return value, count
}
