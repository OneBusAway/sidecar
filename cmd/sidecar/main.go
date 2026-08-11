// Command sidecar is the OneBusAway sidecar server.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/OneBusAway/sidecar/internal/greeting"
)

func main() {
	run(os.Stdout, os.Args[1:])
}

// run holds main's logic so tests can supply their own output and arguments.
func run(out io.Writer, args []string) {
	var name string
	if len(args) > 0 {
		name = args[0]
	}
	fmt.Fprintln(out, greeting.Greet(name))
}
