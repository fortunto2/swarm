package prompts

import (
	"strings"
	"testing"
)

// A box worker ran the whole suite itself (10-25 min there, killed at its 10-minute background
// limit) although the gate then ran it again on the Mac in 2. With the suite elsewhere the
// prompt asks for the static checks and the worker's own tests only.
func TestGateElsewhereAsksForStaticChecksOnly(t *testing.T) {
	w := Worker{ID: 7, Gate: []string{"pnpm test"}, Title: "x", Spec: "s", Acceptance: "a"}
	here, err := RenderWorker(w)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(here, "Run `swarm gate`: the same checks") || strings.Contains(here, "--static") {
		t.Fatalf("gate here: want the full self-check:\n%s", here)
	}
	w.GateElsewhere = "mac"
	there, err := RenderWorker(w)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"runs on mac", "swarm gate --static", "`pnpm test`", "do not run the whole suite"} {
		if !strings.Contains(there, want) {
			t.Fatalf("gate on mac: missing %q in:\n%s", want, there)
		}
	}
	if strings.Contains(there, "Run `swarm gate`: the same checks") {
		t.Fatal("gate elsewhere still asks for the full self-check")
	}
}
