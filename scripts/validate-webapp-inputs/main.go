// Command validate-webapp-inputs rejects ambiguity before any lossy conversion.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
	"gopkg.in/yaml.v3"
)

const maxSourceBytes = 1 << 20

var errSource = errors.New("WebApp source validation refused")

func checkYAML(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	documents := 0
	for {
		var value any
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			if documents == 0 {
				return errSource
			}
			return nil
		}
		if err != nil || documents >= 1024 {
			return errSource
		}
		documents++
	}
}

func checkJSON(data []byte, contract string) error {
	if strictjson.Validate(data, nil) != nil {
		return errSource
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "jq", "--exit-status", "--slurp", "--from-file", contract)
	command.Stdin = bytes.NewReader(data)
	// The validator never emits candidate data or parser diagnostics.
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if command.Run() != nil {
		return errSource
	}
	return nil
}

func run(args []string, stdin io.Reader) error {
	var source, contract string
	switch len(args) {
	case 2:
		if args[0] != "yaml" {
			return errSource
		}
		source = args[1]
	case 3:
		if args[0] != "json" {
			return errSource
		}
		source, contract = args[1], args[2]
	default:
		return errSource
	}
	input := stdin
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return errSource
		}
		defer func() { _ = file.Close() }() // Read-only descriptor; no private copy is owned.
		input = file
	}
	data, err := io.ReadAll(io.LimitReader(input, maxSourceBytes+1))
	if err != nil || len(data) > maxSourceBytes {
		return errSource
	}
	if len(args) == 2 {
		return checkYAML(data)
	}
	return checkJSON(data, contract)
}

func main() {
	if run(os.Args[1:], os.Stdin) != nil {
		fmt.Fprintln(os.Stderr, errSource)
		os.Exit(1)
	}
}
