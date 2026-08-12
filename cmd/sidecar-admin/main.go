// Command sidecar-admin authors service alerts and manages regions by
// writing directly to the sidecar database through the same store package
// the server reads from.
package main

import (
	"fmt"
	"os"

	// time/tzdata embeds the IANA timezone database into the binary, so
	// time.LoadLocation works in a scratch container with no system tzdata
	// -- `region set --timezone` depends on it to validate at the point of
	// the mistake rather than failing later inside `alert list`.
	_ "time/tzdata"
)

func main() {
	if err := run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar-admin:", err)
		os.Exit(1)
	}
}
