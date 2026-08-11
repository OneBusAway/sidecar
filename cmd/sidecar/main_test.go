package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no arguments", args: nil, want: "Hello, world!\n"},
		{name: "name argument", args: []string{"Sidecar"}, want: "Hello, Sidecar!\n"},
		{name: "extra arguments ignored", args: []string{"Sidecar", "extra"}, want: "Hello, Sidecar!\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			run(&buf, tt.args)

			if got := buf.String(); got != tt.want {
				t.Errorf("run(%q) wrote %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
