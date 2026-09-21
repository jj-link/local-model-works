// Package sourceconfig describes byte-bound adaptations of pinned upstream source.
package sourceconfig

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxSourceBytes = 16 << 20
const MaxReplacementBytes = 1 << 20

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Edits  []Edit `json:"edits"`
}

type Edit struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Parameter string `json:"parameter,omitempty"`
	Template  string `json:"template,omitempty"`
	Variable  string `json:"variable,omitempty"`
	Command   string `json:"command,omitempty"`
	Indirect  bool   `json:"indirect,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Suffix    string `json:"suffix,omitempty"`
	Format    string `json:"format"`
	Flag      string `json:"flag,omitempty"`
}

type ResolvedFile struct {
	Path   string         `json:"path"`
	SHA256 string         `json:"sha256"`
	Edits  []ResolvedEdit `json:"edits"`
}

type ResolvedEdit struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement"`
}

var parameterName = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,62}$`)
var flagName = regexp.MustCompile(`^--?[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// SafePath accepts only canonical, portable paths below the recipe source root.
func SafePath(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) || path.IsAbs(value) || path.Clean(value) != value || value == "." || strings.ContainsAny(value, "\\:") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." || component == ".git" || strings.TrimRight(component, ". ") != component {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

// Validate checks structural bounds; source byte length and declared parameter
// references are checked by the source consumer and recipe validator respectively.
func Validate(files []File) error {
	if len(files) > 64 {
		return fmt.Errorf("configuration exceeds 64 files")
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if !SafePath(file.Path) || seen[file.Path] || !validHash(file.SHA256) {
			return fmt.Errorf("configuration file path/hash invalid or duplicated: %q", file.Path)
		}
		seen[file.Path] = true
		if len(file.Edits) == 0 || len(file.Edits) > 2048 {
			return fmt.Errorf("configuration edits invalid: %s", file.Path)
		}
		edits := append([]Edit(nil), file.Edits...)
		sort.Slice(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
		end, start := 0, -1
		for _, edit := range edits {
			if edit.Start < end || edit.Start == start || edit.End < edit.Start || edit.End > MaxSourceBytes {
				return fmt.Errorf("configuration edit range invalid: %s", file.Path)
			}
			bindings := 0
			for _, binding := range [...]string{edit.Parameter, edit.Template, edit.Variable, edit.Command} {
				if binding != "" {
					bindings++
				}
			}
			if bindings != 1 {
				return fmt.Errorf("configuration edit requires exactly one parameter, template, variable or command: %s", file.Path)
			}
			if edit.Parameter != "" && !parameterName.MatchString(edit.Parameter) {
				return fmt.Errorf("configuration edit parameter invalid: %s", file.Path)
			}
			if edit.Variable != "" && (len(edit.Variable) > 256 || !environmentName.MatchString(edit.Variable) || edit.Format != "shell-word") {
				return fmt.Errorf("configuration shell variable invalid: %s", file.Path)
			}
			if edit.Command != "" && (len(edit.Command) > 256 || !environmentName.MatchString(edit.Command) || edit.Format != "shell-word") {
				return fmt.Errorf("configuration shell command invalid: %s", file.Path)
			}
			if edit.Variable == "" && (edit.Prefix != "" || edit.Suffix != "" || edit.Indirect) {
				return fmt.Errorf("configuration affixes and indirection require a shell variable: %s", file.Path)
			}
			if len(edit.Prefix)+len(edit.Suffix) > MaxReplacementBytes || !utf8.ValidString(edit.Prefix) || !utf8.ValidString(edit.Suffix) || strings.ContainsRune(edit.Prefix, 0) || strings.ContainsRune(edit.Suffix, 0) {
				return fmt.Errorf("configuration variable affixes invalid: %s", file.Path)
			}
			if len(edit.Template) > MaxReplacementBytes || !utf8.ValidString(edit.Template) || strings.ContainsRune(edit.Template, 0) {
				return fmt.Errorf("configuration edit template invalid: %s", file.Path)
			}
			start, end = edit.Start, edit.End
			switch edit.Format {
			case "shell", "shell-reparse", "shell-heredoc", "shell-quoted-heredoc", "shell-double-quoted", "shell-compose", "json-compose", "argv", "argv-heredoc", "argv-quoted-heredoc":
				if edit.Flag != "" {
					return fmt.Errorf("configuration flag requires flag format")
				}
			case "shell-word":
				if (edit.Variable == "" && edit.Command == "") || edit.Flag != "" {
					return fmt.Errorf("configuration shell-word requires a variable or command without a flag")
				}
			case "flag", "flag-compose":
				if !flagName.MatchString(edit.Flag) {
					return fmt.Errorf("configuration flag invalid")
				}
			default:
				return fmt.Errorf("configuration format invalid: %q", edit.Format)
			}
		}
	}
	return nil
}

// Argv decodes a JSON string array, never shell syntax. Null is not an array.
func Argv(value any) ([]string, error) {
	text, ok := value.(string)
	if !ok || len(text) > MaxReplacementBytes || !utf8.ValidString(text) {
		return nil, fmt.Errorf("argv must be a JSON string array")
	}
	var values []any
	if err := json.Unmarshal([]byte(text), &values); err != nil || values == nil {
		return nil, fmt.Errorf("argv must be a JSON string array")
	}
	args := make([]string, len(values))
	for i, value := range values {
		arg, ok := value.(string)
		if !ok || strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("argv contains invalid literal")
		}
		args[i] = arg
	}
	return args, nil
}

func quote(value string) (string, error) {
	if len(value) > MaxReplacementBytes || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
		return "", fmt.Errorf("configuration literal invalid or too large")
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'", nil
}

func IsArgvFormat(format string) bool {
	return format == "argv" || format == "argv-heredoc" || format == "argv-quoted-heredoc"
}

// CheckForbiddenArgs enforces only the source-bound option policy supplied by
// the recipe. Engine parsers may abbreviate long options and accept underscores;
// Docker policy leaves engineAliases false and requires full option names.
func CheckForbiddenArgs(args, forbidden []string, engineAliases bool) error {
	if len(forbidden) > 128 {
		return fmt.Errorf("forbiddenArgs exceeds 128 options")
	}
	type protectedOption struct{ flag, canonical string }
	options := make([]protectedOption, 0, len(forbidden))
	seen := make(map[string]bool, len(forbidden))
	for _, flag := range forbidden {
		if len(flag) > 128 || !flagName.MatchString(flag) || seen[flag] {
			return fmt.Errorf("forbiddenArgs contains an invalid or duplicate option: %q", flag)
		}
		seen[flag] = true
		canonical := flag
		if engineAliases && strings.HasPrefix(flag, "--") {
			canonical = strings.ReplaceAll(flag, "_", "-")
		}
		options = append(options, protectedOption{flag: flag, canonical: canonical})
	}
	for _, arg := range args {
		option, _, _ := strings.Cut(arg, "=")
		canonical := option
		if engineAliases && strings.HasPrefix(option, "--") {
			canonical = strings.ReplaceAll(option, "_", "-")
		}
		for _, protected := range options {
			matches := canonical == protected.canonical
			if engineAliases && len(canonical) > 2 && strings.HasPrefix(canonical, "--") && strings.HasPrefix(protected.canonical, "--") {
				matches = matches || strings.HasPrefix(protected.canonical, canonical)
			}
			if len(protected.flag) == 2 && len(option) > 1 && option[0] == '-' && option[1] != '-' {
				matches = matches || strings.Contains(option[1:], protected.flag[1:])
			}
			if matches {
				return fmt.Errorf("argument %q conflicts with controller-managed option %s; use full unambiguous options or dedicated configuration controls", arg, protected.flag)
			}
		}
	}
	return nil
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckDockerArgs excludes positional image/command injection without guessing
// Docker option arities. Every option must carry its own value in the same token.
func CheckDockerArgs(args, forbiddenEnv []string) error {
	if len(forbiddenEnv) > 128 {
		return fmt.Errorf("forbiddenEnv exceeds 128 keys")
	}
	protected := make(map[string]bool, len(forbiddenEnv))
	for _, key := range forbiddenEnv {
		if len(key) > 256 || !environmentName.MatchString(key) || protected[key] {
			return fmt.Errorf("forbiddenEnv contains an invalid or duplicate key: %q", key)
		}
		protected[key] = true
	}
	for _, arg := range args {
		option, value, hasValue := strings.Cut(arg, "=")
		if !hasValue || !strings.HasPrefix(option, "--") || !flagName.MatchString(option) {
			return fmt.Errorf("Docker extra arguments require --long-option=value tokens, not %q", arg)
		}
		if option == "--env-file" {
			return fmt.Errorf("Docker --env-file cannot preserve controller-managed environment bindings")
		}
		if option == "--env" {
			key, _, _ := strings.Cut(value, "=")
			if !environmentName.MatchString(key) {
				return fmt.Errorf("Docker --env requires NAME or NAME=value")
			}
			if protected[key] {
				return fmt.Errorf("Docker environment %s is controller-managed", key)
			}
		}
	}
	return nil
}

var heredocEscape = strings.NewReplacer("\\", "\\\\", "$", "\\$", "`", "\\`")
var doubleQuoteEscape = strings.NewReplacer("\\", "\\\\", "$", "\\$", "`", "\\`", "\"", "\\\"")

func quoteArgv(value, format string) (string, error) {
	quoted, err := quote(value)
	if err != nil || format == "argv" {
		return quoted, err
	}
	quoted = deferLineBreaks(quoted)
	if format == "argv-heredoc" {
		// Unquoted heredocs perform exactly one outer expansion phase.
		quoted = heredocEscape.Replace(quoted)
	}
	return quoted, nil
}

// deferLineBreaks keeps data from terminating a source heredoc or YAML block.
func deferLineBreaks(quoted string) string {
	quoted = strings.ReplaceAll(quoted, "\n", "'$'\\n''")
	return strings.ReplaceAll(quoted, "\r", "'$'\\r''")
}

func scalar(value any, format string) (string, error) {
	quoted, err := literal(value)
	if err != nil || format == "shell" {
		return quoted, err
	}
	quoted = deferLineBreaks(quoted)
	switch format {
	case "shell-reparse":
		return quote(quoted)
	case "shell-heredoc":
		return heredocEscape.Replace(quoted), nil
	case "shell-double-quoted":
		return doubleQuoteEscape.Replace(quoted), nil
	case "shell-compose":
		return strings.ReplaceAll(quoted, "$", "$$"), nil
	default: // Quoted heredoc: the generated shell is the only expansion phase.
		return quoted, nil
	}
}

// computedWord serializes a computed upstream word at its existing reparse
// boundary. Names are validated identifiers; affixes remain literal data.
func computedWord(edit Edit) (string, error) {
	if edit.Command != "" {
		return fmt.Sprintf("$(printf '%%q' \"$(%s)\")", edit.Command), nil
	}
	prefix, err := quoteArgv(edit.Prefix, "argv-quoted-heredoc")
	if err != nil {
		return "", err
	}
	suffix, err := quoteArgv(edit.Suffix, "argv-quoted-heredoc")
	if err != nil {
		return "", err
	}
	variable := edit.Variable
	if edit.Indirect {
		variable = "!" + variable + ":-"
	}
	return fmt.Sprintf("$(printf '%%q' %s\"${%s}\"%s)", prefix, variable, suffix), nil
}

func literal(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return quote(v)
	case bool:
		return quote(strconv.FormatBool(v))
	case int:
		return quote(strconv.Itoa(v))
	case int64:
		return quote(strconv.FormatInt(v, 10))
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return quote(strconv.FormatFloat(v, 'g', -1, 64))
		}
	case json.Number:
		if f, err := v.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return quote(v.String())
		}
	}
	return "", fmt.Errorf("configuration requires a scalar literal")
}

// Resolve omits absent parameter overrides, preserving pinned source bytes.
// Templates are required bindings, rendered before shell or argv encoding.
func Resolve(files []File, values map[string]any, render func(string) (string, error)) ([]ResolvedFile, error) {
	if err := Validate(files); err != nil {
		return nil, err
	}
	var out []ResolvedFile
	for _, file := range files {
		resolved := ResolvedFile{Path: file.Path, SHA256: strings.ToLower(file.SHA256)}
		for _, edit := range file.Edits {
			var value any
			var err error
			binding := "parameter " + edit.Parameter
			if edit.Variable != "" || edit.Command != "" {
				replacement, err := computedWord(edit)
				if err != nil {
					return nil, fmt.Errorf("configuration computed word: %w", err)
				}
				resolved.Edits = append(resolved.Edits, ResolvedEdit{Start: edit.Start, End: edit.End, Replacement: replacement})
				continue
			}
			if edit.Template != "" {
				binding = "template"
				if render == nil {
					return nil, fmt.Errorf("configuration template requires a renderer: %s", file.Path)
				}
				value, err = render(edit.Template)
				if err != nil {
					return nil, fmt.Errorf("configuration template: %w", err)
				}
			} else {
				var exists bool
				value, exists = values[edit.Parameter]
				if !exists {
					continue
				}
			}
			var replacement string
			switch edit.Format {
			case "shell", "shell-reparse", "shell-heredoc", "shell-quoted-heredoc", "shell-double-quoted", "shell-compose":
				replacement, err = scalar(value, edit.Format)
			case "json-compose":
				text, ok := value.(string)
				if !ok || len(text) > MaxReplacementBytes || strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
					err = fmt.Errorf("configuration requires a valid string literal")
				} else {
					var encoded []byte
					encoded, err = json.Marshal(text)
					replacement = strings.ReplaceAll(string(encoded), "$", "$$")
				}
			case "flag", "flag-compose":
				flag, ok := value.(bool)
				if edit.Template != "" {
					switch value {
					case "true":
						flag, ok = true, true
					case "false":
						flag, ok = false, true
					}
				}
				if !ok {
					err = fmt.Errorf("flag requires bool")
				} else if flag {
					replacement = edit.Flag
				} else if edit.Format == "flag-compose" {
					// A blank folded-YAML line would split the shell command.
					// This unquoted expansion always produces zero argv words.
					replacement = "$${0:+}"
				}
			case "argv", "argv-heredoc", "argv-quoted-heredoc":
				var args []string
				args, err = Argv(value)
				if err == nil {
					for i, arg := range args {
						args[i], err = quoteArgv(arg, edit.Format)
						if err != nil {
							break
						}
					}
					replacement = " "
					if len(args) != 0 {
						replacement += strings.Join(args, " ") + " "
					}
				}
			}
			if err != nil {
				return nil, fmt.Errorf("configuration %s: %w", binding, err)
			}
			resolved.Edits = append(resolved.Edits, ResolvedEdit{Start: edit.Start, End: edit.End, Replacement: replacement})
		}
		if len(resolved.Edits) != 0 {
			out = append(out, resolved)
		}
	}
	if err := ValidateResolved(out); err != nil {
		return nil, err
	}
	return out, nil
}

func ValidateResolved(files []ResolvedFile) error {
	definitions := make([]File, len(files))
	for i, file := range files {
		definitions[i] = File{Path: file.Path, SHA256: file.SHA256}
		for _, edit := range file.Edits {
			if len(edit.Replacement) > MaxReplacementBytes || strings.ContainsRune(edit.Replacement, 0) || !utf8.ValidString(edit.Replacement) {
				return fmt.Errorf("configuration replacement invalid")
			}
			definitions[i].Edits = append(definitions[i].Edits, Edit{Start: edit.Start, End: edit.End, Parameter: "resolved", Format: "shell"})
		}
	}
	return Validate(definitions)
}

// Apply returns a new file without touching any original byte outside the edits.
func Apply(original []byte, edits []ResolvedEdit) ([]byte, error) {
	if len(original) > MaxSourceBytes || !utf8.Valid(original) {
		return nil, fmt.Errorf("configuration source invalid or too large")
	}
	ordered := append([]ResolvedEdit(nil), edits...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })
	out := make([]byte, 0, len(original))
	end, start := 0, -1
	for _, edit := range ordered {
		if edit.Start < end || edit.Start == start || edit.End < edit.Start || edit.End > len(original) || (edit.Start < len(original) && !utf8.RuneStart(original[edit.Start])) || (edit.End < len(original) && !utf8.RuneStart(original[edit.End])) {
			return nil, fmt.Errorf("configuration edit outside source byte boundaries")
		}
		out = append(out, original[end:edit.Start]...)
		out = append(out, edit.Replacement...)
		if len(out) > MaxSourceBytes {
			return nil, fmt.Errorf("configured source too large")
		}
		start, end = edit.Start, edit.End
	}
	out = append(out, original[end:]...)
	if len(out) > MaxSourceBytes {
		return nil, fmt.Errorf("configured source too large")
	}
	return out, nil
}
