package compose

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// LoadDotEnv parses a .env file at path and returns its contents as a map.
// If the file does not exist, (nil, nil) is returned — a missing .env is not
// an error; many compose projects have no variables to interpolate.
//
// Supported syntax:
//
//	# comment
//	KEY=value
//	KEY="quoted value with  spaces and \n escapes"
//	KEY='raw single-quoted value (no escapes)'
//	KEY=      (empty value)
//	KEY=value # trailing comment
//
// Lines without '=' are rejected.  Keys must start with a letter or underscore
// and contain only [A-Za-z0-9_].
func LoadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return parseDotEnv(f, path)
}

// ParseDotEnv parses reader as a .env document. Useful for tests and when
// the caller already has the bytes in memory.
func ParseDotEnv(r io.Reader) (map[string]string, error) {
	return parseDotEnv(r, "")
}

func parseDotEnv(r io.Reader, path string) (map[string]string, error) {
	vars := make(map[string]string)
	scanner := bufio.NewScanner(r)
	// Bump the default 64 KiB line limit to accept reasonably large values.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		line := strings.TrimLeft(raw, " \t")

		// Blank / comment lines.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Optional "export " prefix, as `.env` files sometimes borrow shell syntax.
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimPrefix(line, "export ")
			line = strings.TrimLeft(line, " \t")
		}

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmtDotEnvError(path, lineNum, "expected KEY=VALUE")
		}

		key := strings.TrimSpace(line[:eq])
		if !isValidKey(key) {
			return nil, fmtDotEnvError(path, lineNum, fmt.Sprintf("invalid key %q", key))
		}

		value, err := parseValue(line[eq+1:])
		if err != nil {
			return nil, fmtDotEnvError(path, lineNum, err.Error())
		}

		vars[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return vars, nil
}

// parseValue extracts a value from the right-hand side of an assignment.
// It handles three forms:
//
//   - "double quoted"  — supports \n \r \t \\ \" escapes
//   - 'single quoted'  — literal, no escapes
//   - unquoted         — trimmed, trailing ` # comment` stripped
func parseValue(s string) (string, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", nil
	}

	switch s[0] {
	case '"':
		return parseDoubleQuoted(s)
	case '\'':
		return parseSingleQuoted(s)
	default:
		return parseUnquoted(s), nil
	}
}

func parseDoubleQuoted(s string) (string, error) {
	var b strings.Builder
	escape := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		if escape {
			switch c {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			default:
				b.WriteByte('\\')
				b.WriteByte(c)
			}
			escape = false
			continue
		}
		if c == '\\' {
			escape = true
			continue
		}
		if c == '"' {
			// Anything after the closing quote must be whitespace or a # comment.
			rest := strings.TrimSpace(s[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", fmt.Errorf("unexpected text after closing quote: %q", rest)
			}
			return b.String(), nil
		}
		b.WriteByte(c)
	}
	return "", fmt.Errorf("unterminated double-quoted string")
}

func parseSingleQuoted(s string) (string, error) {
	end := strings.IndexByte(s[1:], '\'')
	if end < 0 {
		return "", fmt.Errorf("unterminated single-quoted string")
	}
	rest := strings.TrimSpace(s[1+end+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected text after closing quote: %q", rest)
	}
	return s[1 : 1+end], nil
}

// parseUnquoted trims trailing whitespace and strips a trailing `  # comment`
// while leaving `#` characters inside the value alone (e.g. a URL fragment).
// It keeps the docker-compose rule: a `#` only starts a comment when it is
// preceded by whitespace.
func parseUnquoted(s string) string {
	out := s
	// Find the first ` #` (space-or-tab followed by #) not preceded by another #.
	for i := 0; i < len(out); i++ {
		if (out[i] == ' ' || out[i] == '\t') && i+1 < len(out) && out[i+1] == '#' {
			out = out[:i]
			break
		}
	}
	return strings.TrimRight(strings.TrimLeft(out, " \t"), " \t")
}

func isValidKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		if i == 0 {
			if !(r == '_' || unicode.IsLetter(r)) {
				return false
			}
			continue
		}
		if !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

func fmtDotEnvError(path string, line int, msg string) error {
	if path == "" {
		return fmt.Errorf("dotenv line %d: %s", line, msg)
	}
	return fmt.Errorf("%s:%d: %s", path, line, msg)
}
