// Command guard-trivy-runtime-dispositions verifies exact runtime scanner premises.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/devantler-tech/platform/internal/trivyruntimedispositions"
)

func main() {
	root := flag.String("root", ".", "repository source root")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "no positional arguments permitted")
		os.Exit(2)
	}
	if err := trivyruntimedispositions.Validate(*root); err != nil {
		fmt.Fprintf(os.Stderr, "Runtime disposition premise refused: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("Runtime dispositions: exact scope, images, source, ownership, ENV/ESO and projected-token/RBAC contracts verified")
}
