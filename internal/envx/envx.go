// Package envx builds the environment for processes swarm starts on someone else's behalf:
// workers and gate commands. They get the operator's environment minus anything that names
// an account, a billing route or a credential; a provider that needs one sets it itself.
package envx

import (
	"os"
	"strings"
)

var dropExact = map[string]bool{
	"CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true, "SWARM_PROFILE": true,
	"SSH_AUTH_SOCK": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
}

// Prefixes that route a CLI to another account or bill, or carry a credential.
var dropPrefix = []string{"ANTHROPIC_", "CLAUDE_CODE_", "OPENAI_", "AWS_", "GH_", "GITHUB_", "GOOGLE_APPLICATION_", "AZURE_"}

// Dropped reports whether a variable is withheld from workers and gates.
func Dropped(key string) bool {
	if dropExact[key] {
		return true
	}
	for _, p := range dropPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Clean is os.Environ() without the dropped variables.
func Clean() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !Dropped(k) {
			out = append(out, kv)
		}
	}
	return out
}

// keepExact and keepPrefix: what a worker or gate needs to run toolchains, nothing else.
// An allowlist: a credential with an unusual name (NPM_TOKEN, STRIPE_KEY, DATABASE_URL) is
// simply not in it.
var keepExact = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true,
	"LANG": true, "TMPDIR": true, "TZ": true, "PWD": true, "COLORTERM": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
}

var keepPrefix = []string{"LC_", "XDG_", "GO", "CGO_", "RUSTUP_", "JAVA_", "PYTHON", "VIRTUAL_ENV", "NVM_", "NODE_OPTIONS", "DEVELOPER_DIR", "SDKROOT"}

// Minimal is os.Environ() reduced to the allowlist.
func Minimal() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if Dropped(k) {
			continue
		}
		keep := keepExact[k]
		for _, p := range keepPrefix {
			keep = keep || strings.HasPrefix(k, p)
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}
