package workerssh

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	maxConfigFiles = 64
	maxConfigBytes = 1 << 20
	maxAliases     = 128
)

type configRoot struct {
	path string
	base string
}

func configRoots() ([]configRoot, string, error) {
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		return nil, "", fmt.Errorf("cannot determine SSH account home directory: %v", err)
	}
	account, err = user.LookupId(account.Uid)
	if err != nil || account.HomeDir == "" {
		return nil, "", fmt.Errorf("cannot resolve SSH account home directory: %v", err)
	}
	home := account.HomeDir
	system := "/etc/ssh"
	if runtime.GOOS == "windows" {
		system = filepath.Join(os.Getenv("PROGRAMDATA"), "ssh")
		if !filepath.IsAbs(system) {
			return nil, "", fmt.Errorf("cannot determine system SSH configuration directory")
		}
	}
	return []configRoot{
		{filepath.Join(home, ".ssh", "config"), filepath.Join(home, ".ssh")},
		{filepath.Join(system, "ssh_config"), system},
	}, home, nil
}

// discoverAliases only enumerates literal candidates; OpenSSH, not this scanner,
// evaluates Host/Match conditions and computes their effective configuration.
// Includes are scanned even in inactive blocks so Match exec cannot run during -G.
func discoverAliases(roots []configRoot, home string) ([]string, error) {
	var aliases []string
	seenAliases := make(map[string]bool)
	active := make(map[string]bool)
	files, size := 0, 0
	var visit func(string, string, int) error
	visit = func(path, base string, depth int) error {
		if depth > 8 || files >= maxConfigFiles {
			return fmt.Errorf("SSH configuration exceeds include depth/file limit")
		}
		path = filepath.Clean(path)
		if active[path] {
			return fmt.Errorf("SSH configuration include cycle at %q", path)
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read SSH configuration %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("SSH configuration %q is not a regular file", path)
		}
		if info.Size() > int64(maxConfigBytes-size) {
			return fmt.Errorf("SSH configuration exceeds byte limit")
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("read SSH configuration %q: %w", path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(maxConfigBytes-size)+1))
		file.Close()
		if readErr != nil {
			return fmt.Errorf("read SSH configuration %q: %w", path, readErr)
		}
		size += len(data)
		if size > maxConfigBytes {
			return fmt.Errorf("SSH configuration exceeds byte limit")
		}
		files++
		active[path] = true
		defer delete(active, path)
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			fields, err := configFields(scanner.Text())
			if err != nil {
				return fmt.Errorf("SSH configuration %q: %w", path, err)
			}
			if len(fields) < 2 {
				continue
			}
			switch strings.ToLower(fields[0]) {
			case "match":
				for _, field := range fields[1:] {
					if strings.EqualFold(strings.TrimPrefix(field, "!"), "exec") {
						return fmt.Errorf("SSH configuration %q uses Match exec; read-only worker SSH checks cannot execute local commands", path)
					}
				}
			case "host":
				for _, alias := range fields[1:] {
					if !validHost(alias) || strings.ContainsAny(alias, "[]:") || seenAliases[alias] {
						continue
					}
					if len(aliases) >= maxAliases {
						return fmt.Errorf("SSH configuration exceeds literal Host alias limit")
					}
					seenAliases[alias] = true
					aliases = append(aliases, alias)
				}
			case "include":
				for _, pattern := range fields[1:] {
					pattern, err = expandInclude(pattern, home, base)
					if err != nil {
						return err
					}
					matches, err := includeGlob(pattern)
					if err != nil {
						return fmt.Errorf("invalid SSH Include pattern %q: %w", pattern, err)
					}
					if len(matches) > maxConfigFiles-files {
						return fmt.Errorf("SSH configuration exceeds include file limit")
					}
					for _, match := range matches {
						if err := visit(match, base, depth+1); err != nil {
							return err
						}
					}
				}
			}
		}
		return scanner.Err()
	}
	for _, root := range roots {
		if err := visit(root.path, root.base, 0); err != nil {
			return nil, err
		}
	}
	return aliases, nil
}

func expandInclude(pattern, home, base string) (string, error) {
	if len(pattern) > 4096 {
		return "", fmt.Errorf("SSH Include path exceeds length limit")
	}
	// OpenSSH expands ${NAME}, not bare $NAME, in Include paths.
	for start := strings.Index(pattern, "${"); start >= 0; start = strings.Index(pattern, "${") {
		end := strings.IndexByte(pattern[start:], '}')
		if end < 0 {
			return "", fmt.Errorf("invalid environment expansion in SSH Include")
		}
		end += start
		value, ok := os.LookupEnv(pattern[start+2 : end])
		if !ok || strings.Contains(value, "${") {
			return "", fmt.Errorf("unavailable or recursive environment expansion in SSH Include")
		}
		pattern = pattern[:start] + value + pattern[end+1:]
		if len(pattern) > 4096 {
			return "", fmt.Errorf("SSH Include path exceeds length limit")
		}
	}
	// Target-dependent tokens cannot be enumerated without reimplementing
	// OpenSSH's conditional interpreter. Fail closed rather than miss a file
	// containing Match exec that ssh -G would otherwise execute.
	if strings.Contains(pattern, "%") {
		return "", fmt.Errorf("token-expanded SSH Include paths are unsupported by read-only worker SSH checks")
	}
	if strings.HasPrefix(pattern, "~") {
		name, rest, _ := strings.Cut(pattern[1:], "/")
		if name != "" {
			account, err := user.Lookup(name)
			if err != nil {
				return "", fmt.Errorf("resolve SSH Include account %q: %w", name, err)
			}
			home = account.HomeDir
		}
		pattern = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(base, pattern)
	}
	return pattern, nil
}

// OpenSSH uses POSIX glob(3). Translate its common negated bracket spelling;
// fail closed for locale-specific classes Go's filepath matcher cannot model.
func includeGlob(pattern string) ([]string, error) {
	for _, unsupported := range []string{"[:", "[.", "[=", "[]", "[!]", "[^]"} {
		if strings.Contains(pattern, unsupported) {
			return nil, fmt.Errorf("unsupported bracket expression in read-only worker SSH checks")
		}
	}
	patternBytes := []byte(pattern)
	escaped := false
	for i, c := range patternBytes {
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && runtime.GOOS != "windows" {
			escaped = true
			continue
		}
		if c == '[' && i+1 < len(patternBytes) && patternBytes[i+1] == '!' {
			patternBytes[i+1] = '^'
		}
	}
	return filepath.Glob(string(patternBytes))
}

// configFields supports OpenSSH's whitespace/equals separator, quotes and escapes.
// It is deliberately not a Host/Match interpreter.
func configFields(line string) ([]string, error) {
	var fields []string
	var word strings.Builder
	var quote byte
	escaped, started := false, false
	equals := false
	flush := func() {
		if started {
			fields = append(fields, word.String())
			word.Reset()
			started = false
		}
	}
	for i := range len(line) {
		c := line[i]
		if escaped {
			word.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			started = true
			if i+1 < len(line) && (line[i+1] == '\'' || line[i+1] == '"' || line[i+1] == '\\' || quote == 0 && line[i+1] == ' ') {
				escaped = true
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote, started = c, true
			continue
		}
		if c == '#' && !started {
			break
		}
		if c == '=' && !equals && (len(fields) == 0 || len(fields) == 1 && !started) {
			equals = true
			flush()
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			flush()
			continue
		}
		started = true
		word.WriteByte(c)
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated SSH configuration quote or escape")
	}
	flush()
	return fields, nil
}
