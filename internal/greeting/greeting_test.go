package greeting_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/greeting"
)

func TestGreet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "named", input: "Sidecar", want: "Hello, Sidecar!"},
		{name: "empty falls back to world", input: "", want: "Hello, world!"},
		{name: "whitespace is preserved verbatim", input: " ", want: "Hello,  !"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := greeting.Greet(tt.input); got != tt.want {
				t.Errorf("Greet(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
