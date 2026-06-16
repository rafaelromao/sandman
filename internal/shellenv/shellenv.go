// Package shellenv builds a shell command string with a validated, single-quoted
// environment prefix suitable for `sh -c`.
//
// Every env-map key is checked against POSIX shell-variable naming rules
// (^[A-Za-z_][A-Za-z0-9_]*$); bad keys are surfaced as a typed *InvalidKeyError
// listing every rejection so callers can refuse the whole batch instead of
// silently dropping dangerous entries. Values are emitted unquoted when they
// contain only shell-safe ASCII and single-quoted with the `'\”` idiom
// otherwise, so embedded whitespace, single quotes, and other shell metachars
// are inert when the result is handed to `sh -c`.
package shellenv

import (
	"regexp"
	"sort"
	"strings"
)

var validKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateKey reports whether key is a safe shell environment variable name.
// Allowed: ASCII letters, digits, and underscores, starting with a letter or
// underscore.
func ValidateKey(key string) error {
	if !validKeyPattern.MatchString(key) {
		return &InvalidKeyError{Keys: []string{key}}
	}
	return nil
}

// Build returns a shell command string that exports every entry in env (sorted
// by key, each value single-quoted) and then runs cmd. The output is suitable
// for handing to `sh -c`. When env is empty, cmd is returned unchanged. If any
// key fails validation, Build returns an empty string and a *InvalidKeyError
// listing every rejected key.
func Build(env map[string]string, cmd string) (string, error) {
	if len(env) == 0 {
		return cmd, nil
	}
	keys := make([]string, 0, len(env))
	var bad []string
	for k := range env {
		if validKeyPattern.MatchString(k) {
			keys = append(keys, k)
		} else {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		return "", &InvalidKeyError{Keys: bad}
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(renderValue(env[k]))
	}
	b.WriteString("; ")
	b.WriteString(cmd)
	return b.String(), nil
}

// renderValue returns the shell-safe form of a value. Simple values made of
// shell-safe characters are emitted unquoted; values that contain whitespace,
// quotes, or other shell-special characters are wrapped in single quotes with
// the `'\”` escape idiom.
func renderValue(value string) string {
	if value == "" {
		return "''"
	}
	if isSafeUnquoted(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// Quote always emits value as a single-quoted shell token, using the
// `'\”` idiom to escape embedded single quotes. Use it when constructing
// shell command arguments that must be quoted regardless of the
// value's contents (for example, a branch name interpolated into a
// `git` invocation). For env-map values, prefer Build.
func Quote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func isSafeUnquoted(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '+' || r == ',' || r == '@' || r == '=':
		default:
			return false
		}
	}
	return true
}

// InvalidKeyError is returned by Build when one or more env-map keys fail
// validation. Keys lists every rejected key in unspecified order.
type InvalidKeyError struct {
	Keys []string
}

func (e *InvalidKeyError) Error() string {
	return "shellenv: invalid env key(s): " + strings.Join(e.Keys, ", ")
}
