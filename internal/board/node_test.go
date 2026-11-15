package board

import (
	"encoding/json"
	"testing"
)

// A node and a coordinator one release apart still say hello: the old bare-id fields are
// ignored by the new side (never a cleanup by id alone), and the new fields by the old.
func TestHelloAcrossOneReleaseDecodes(t *testing.T) {
	var h NodeHello
	if err := json.Unmarshal([]byte(`{"name":"mac","root":"/wt","repos":["app"],"worktrees":[1,2]}`), &h); err != nil {
		t.Fatalf("an older node's hello does not decode: %v", err)
	}
	if len(h.Worktrees) != 0 {
		t.Fatalf("bare ids were taken as worktrees: %+v", h.Worktrees)
	}
	var w NodeWelcome
	if err := json.Unmarshal([]byte(`{"session":"s","providers":{},"repos":{},"cleanup":[3]}`), &w); err != nil {
		t.Fatalf("an older coordinator's welcome does not decode: %v", err)
	}
	b, _ := json.Marshal(NodeHello{Worktrees: []TaskRef{{Repo: "app", Task: 1}}})
	var old struct {
		Worktrees []int64 `json:"worktrees"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("an older coordinator cannot decode this hello: %v", err)
	}
}
