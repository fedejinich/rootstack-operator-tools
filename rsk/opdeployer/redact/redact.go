// Package redact provides utilities for redacting secrets from log output.
//
// It is used across all cmd/ tools (deploy-rollup, rollup-monitor, rollup-node,
// batcher, bridge-test) to ensure private keys, RPC credentials, JWT secrets,
// and other sensitive data never appear in log files or console output.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// secretFlags lists CLI flag names whose next positional argument (or =value
// suffix) contains a secret that must be redacted.
var secretFlags = []string{
	"--private-key",
	"--funder-private-key",
	"--batcher-private-key",
	"--proposer-private-key",
	"--master-private-key",
	"--jwt-secret",
	"--mnemonic",
}

// placeholder is the replacement text for redacted values.
const placeholder = "[REDACTED]"

// Command returns a single-line string representation of a command and its
// arguments with secret values replaced by [REDACTED].
//
// It handles two forms:
//   - Flag and value as separate args:  --private-key 0xabcd...
//   - Flag=value combined arg:          --private-key=0xabcd...
func Command(args []string) string {
	redacted := make([]string, 0, len(args))
	skipNext := false

	for i, arg := range args {
		if skipNext {
			redacted = append(redacted, placeholder)
			skipNext = false
			continue
		}

		// Check for --flag=value form
		if idx := strings.IndexByte(arg, '='); idx > 0 {
			flag := arg[:idx]
			if isSecretFlag(flag) {
				redacted = append(redacted, flag+"="+placeholder)
				continue
			}
		}

		// Check for --flag value form (flag is this arg, value is next)
		if isSecretFlag(arg) && i+1 < len(args) {
			redacted = append(redacted, arg)
			skipNext = true
			continue
		}

		redacted = append(redacted, arg)
	}

	return strings.Join(redacted, " ")
}

// URL redacts userinfo (username:password) from a URL string.
//
// For example:
//
//	http://user:pass@host:8545  →  http://***:***@host:8545
//	http://host:8545            →  http://host:8545  (unchanged)
//
// If the string is not a valid URL it is returned unchanged.
func URL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.User == nil {
		return rawURL
	}
	// Reconstruct the URL with redacted userinfo.
	// We can't use url.UserPassword because it percent-encodes special chars.
	userinfo := u.User.String()
	return strings.Replace(rawURL, userinfo+"@", "***:***@", 1)
}

// Key returns a redacted version of a hex private key or secret string.
// It keeps the first 4 and last 4 characters, replacing the middle with "***".
//
// Short strings (8 chars or fewer) are fully replaced with [REDACTED].
// Empty strings are returned as-is.
func Key(s string) string {
	if s == "" {
		return ""
	}
	// Strip 0x prefix for length check, but preserve it in output
	raw := strings.TrimPrefix(s, "0x")
	if len(raw) <= 8 {
		return placeholder
	}
	prefix := ""
	if strings.HasPrefix(s, "0x") {
		prefix = "0x"
	}
	return prefix + raw[:4] + "***" + raw[len(raw)-4:]
}

// Output redacts any long hex strings (32+ hex chars, with optional 0x prefix)
// from command output. This is a safety net for subprocess stderr/stdout that
// might echo back private keys or other hex secrets.
func Output(s string) string {
	// Match 0x-prefixed or bare hex strings of 32+ hex chars (64+ for a 256-bit key)
	return hexPattern.ReplaceAllStringFunc(s, func(match string) string {
		return Key(match)
	})
}

// hexPattern matches hex strings that are likely private keys (64 hex chars,
// optionally prefixed with 0x).
var hexPattern = regexp.MustCompile(`(?i)\b0x[0-9a-f]{64}\b|(?i)\b[0-9a-f]{64}\b`)

// isSecretFlag reports whether the given flag name matches one of the known
// secret-bearing flags (case-insensitive comparison).
func isSecretFlag(flag string) bool {
	lower := strings.ToLower(flag)
	for _, sf := range secretFlags {
		if lower == sf {
			return true
		}
	}
	return false
}
