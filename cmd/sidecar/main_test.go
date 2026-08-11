package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if err := run(&stdout, &stderr, nil); err != nil {
		t.Fatalf("run() returned %v, want nil", err)
	}
	if got, want := stdout.String(), "Hello, world!\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
