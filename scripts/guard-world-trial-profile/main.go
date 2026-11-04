// The World trial proof reads source and public artifacts; it never runs a zone.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/devantler-tech/platform/internal/worldtrialprofile"
)

func main() {
	root := flag.String("root", ".", "source root")
	native := flag.Bool("native", false, "fresh anonymous registry, publisher and complete-render proof")
	evidence := flag.String("evidence", "", "new private evidence directory for native proof")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*root)
	if err == nil && *native {
		if *evidence == "" {
			err = fmt.Errorf("native proof requires -evidence")
		} else {
			var ev string
			ev, err = filepath.Abs(*evidence)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()
				err = worldtrialprofile.VerifyNative(ctx, abs, ev, worldtrialprofile.Tools{Cosign: os.Getenv("COSIGN_BIN"), ORAS: os.Getenv("ORAS_BIN"), Kubectl: os.Getenv("KUBECTL_BIN")})
			}
		}
	} else if err == nil {
		err = worldtrialprofile.Validate(abs)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "World trial profile refused:", err)
		os.Exit(2)
	}
	fmt.Println("PASS: immutable audited World private-trial profile")
}
