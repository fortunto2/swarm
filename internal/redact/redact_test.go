package redact

import "testing"

func TestFilter(t *testing.T) {
	f := Filter{"sed", "s/sk-[a-z0-9]*/[R]/g"}
	if got := f.Apply("key sk-abc123 here"); got != "key [R] here" {
		t.Fatalf("got %q", got)
	}
	if got := (Filter{"false"}).Apply("sk-abc123"); got != Withheld {
		t.Fatalf("a failed filter must withhold, got %q", got)
	}
	if got := (Filter(nil)).Apply("as is"); got != "as is" {
		t.Fatalf("no filter: %q", got)
	}
}
