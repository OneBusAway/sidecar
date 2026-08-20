// Package dotenv loads KEY=VALUE pairs from a .env file into the process
// environment. It exists so a developer's local .env is picked up at boot
// without an aliasing layer or a shell wrapper.
//
// The supported syntax is deliberately minimal: blank lines, full-line #
// comments, and KEY=VALUE pairs whose value may be wrapped in matching
// single or double quotes. Anything else -- including duplicate keys -- is
// an error, on the theory that a loudly rejected file is easier to fix than
// one whose unsupported corners are silently skipped.
package dotenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Load reads the file at path and exports each pair into the process
// environment. Variables already present in the environment -- even
// set-but-empty ones -- always win over the file, so a real deployment's
// platform-provided configuration is never shadowed by a stray .env on
// disk. A missing file is not an error; a malformed one is, and nothing
// from a malformed file is applied.
//
// Errors carry the path and a line number, never the line's content: a
// .env line is exactly where a secret lives, and these errors flow
// straight into the boot log.
func Load(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dotenv: reading %s failed", path)
	}

	// Validate the whole file before touching the environment: a file with
	// an error on line 2 must not have already exported line 1.
	type pair struct{ key, value string }
	var pairs []pair
	seen := make(map[string]bool)
	for i, line := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, rawValue, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("dotenv: %s line %d: not a KEY=VALUE pair", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return fmt.Errorf("dotenv: %s line %d: invalid variable name", path, lineNo)
		}
		if seen[key] {
			return fmt.Errorf("dotenv: %s line %d: duplicate variable %s", path, lineNo, key)
		}
		seen[key] = true

		value, err := unquote(strings.TrimSpace(rawValue))
		if err != nil {
			return fmt.Errorf("dotenv: %s line %d: %w", path, lineNo, err)
		}
		pairs = append(pairs, pair{key, value})
	}

	for _, p := range pairs {
		if _, set := os.LookupEnv(p.key); set {
			continue
		}
		if err := os.Setenv(p.key, p.value); err != nil {
			return fmt.Errorf("dotenv: setting %s failed", p.key)
		}
	}
	return nil
}

// validKey reports whether key is a POSIX-style variable name:
// [A-Za-z_][A-Za-z0-9_]*.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_', 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z':
		case '0' <= r && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unquote strips one pair of matching single or double quotes from value.
// The quoted content is taken verbatim -- no escape sequences, no
// interpolation. A value that opens a quote must close it.
func unquote(value string) (string, error) {
	if value == "" || (value[0] != '"' && value[0] != '\'') {
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != value[0] {
		return "", errors.New("unterminated quoted value")
	}
	return value[1 : len(value)-1], nil
}
