package commands

import (
	"bytes"
	"testing"
)

func TestPrintPending(t *testing.T) {
	var out bytes.Buffer
	if err := printPending(&out, []byte(`[{"id":"req-1","persona":"security","tool":"Bash"}]`)); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "req-1  security wants Bash\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintPendingEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := printPending(&out, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "(none)\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
