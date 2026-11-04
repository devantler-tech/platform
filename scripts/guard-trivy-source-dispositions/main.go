// Command guard-trivy-source-dispositions checks the premises of narrow Trivy dispositions.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/devantler-tech/platform/internal/trivydispositions"
)

func main() {
	root := flag.String("root", ".", "repository root to validate")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "no positional arguments are permitted")
		os.Exit(2)
	}
	if err := trivydispositions.Validate(*root); err != nil {
		fmt.Fprintf(os.Stderr, "Trivy disposition premise refused: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("Trivy source dispositions: exact scope, ownership, parsed data and LimitRange properties verified")
}
