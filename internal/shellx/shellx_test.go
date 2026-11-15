package shellx

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The timeout ends a command whose child holds the output pipe (found in review: an admit
// command `sleep 5; echo hi` with a 1 s timeout returned after 5 s through CombinedOutput).
func TestTimeoutKillsTheChildHoldingThePipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Command(ctx, "sleep 20 | cat; echo late").CombinedOutput()
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("returned after %v: the child outlived the timeout", took)
	}
	if err == nil {
		t.Fatal("a killed command reported success")
	}
}

// args reach the command as $1..., quoted by the shell, never spliced into the line.
func TestArgsArePositional(t *testing.T) {
	out, err := Command(context.Background(), `printf '%s' "$1"`, "x", "a b; echo injected").Output()
	if err != nil || string(out) != "a b; echo injected" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if strings.Contains(string(out), "\n") {
		t.Fatal("the argument was run as a command")
	}
}
