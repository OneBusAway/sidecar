// Command sidecar is the OneBusAway sidecar server.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/OneBusAway/sidecar/internal/greeting"
)

func main() {
	if err := run(os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar:", err)
		os.Exit(1)
	}
}

// run holds main's logic so tests can supply their own streams and arguments.
// It returns an error rather than exiting so main owns the only exit path.
//
//nolint:unparam // seam intentionally always returns nil today; later tasks add fallible logic here.
func run(stdout, _ io.Writer, args []string) error {
	var name string
	if len(args) > 0 {
		name = args[0]
	}
	fmt.Fprintln(stdout, greeting.Greet(name))
	return nil
}
