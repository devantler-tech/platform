// Command guard-cel-applicability emits a narrow, source-bound CEL assessment.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/devantler-tech/platform/internal/celapplicability"
)

func main() {
	root := flag.String("root", "", "clean immutable Git checkout also supplied to native SCA")
	flag.Parse()
	if *root == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: guard-cel-applicability --root <immutable-checkout>")
		os.Exit(2)
	}
	proof, err := celapplicability.Prove(context.Background(), *root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CEL applicability UNKNOWN:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(proof); err != nil {
		fmt.Fprintln(os.Stderr, "write CEL proof:", err)
		os.Exit(1)
	}
}
