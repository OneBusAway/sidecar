// Package greeting produces the sidecar's startup greeting.
package greeting

import "fmt"

// Greet returns a greeting addressed to name. An empty name is addressed to
// the world at large.
func Greet(name string) string {
	if name == "" {
		name = "world"
	}
	return fmt.Sprintf("Hello, %s!", name)
}
