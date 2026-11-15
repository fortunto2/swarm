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
