// retire-arc-pool plans compare-and-swap patches only for protected production
// recovery. The Bash orchestrator supplies fresh, privately projected receipts.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func run(action string, in io.Reader, out io.Writer) error {
	ref := os.Getenv("GITHUB_REF")
	owner := identity{os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT"), os.Getenv("GITHUB_SHA")}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || !validIdentity(owner) || (ref != "refs/heads/main" && !strings.HasPrefix(ref, "refs/heads/gh-readonly-queue/main/")) {
		return fmt.Errorf("retirement requires the protected production workflow")
	}
	data, err := io.ReadAll(io.LimitReader(in, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || unambiguous(data) != nil {
		return fmt.Errorf("invalid bounded retirement input")
	}
	if action == "check-drain" {
		return drainProof(data)
	}
	if action == "check-json" {
		return unambiguous(data)
	}
	if action == "check-controller" {
		return controllerProof(data)
	}
	if action == "check-source" {
		return sourceProof(data)
	}
	if action == "check-writer" {
		return writerCurrentProof(data)
	}
	var input struct {
		State       state
		Receipt     json.RawMessage
		WriterProof json.RawMessage
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.State.Owner != owner {
		return fmt.Errorf("retirement input does not belong to this native attempt")
	}
	var patch []operation
	switch action {
	case "claim":
		patch, err = claim(input.State, input.Receipt)
	case "bind":
		patch, err = bindBaseline(input.State)
	case "writer":
		patch, err = recordWriter(input.State, input.WriterProof)
	case "check-writer-barrier":
		var j journal
		j, err = verify(input.State)
		if err == nil {
			err = sourceWriterProof(input.State, j, input.WriterProof)
		}
		return err
	case "inspect":
		var j journal
		j, err = readJournal(input.State)
		if err == nil {
			return json.NewEncoder(out).Encode(j)
		}
	case "verify":
		var j journal
		j, err = verify(input.State)
		if err == nil {
			return json.NewEncoder(out).Encode(j)
		}
	case "verify-delete":
		var j journal
		j, err = verifyDelete(input.State)
		if err == nil {
			return json.NewEncoder(out).Encode(j)
		}
	default:
		patch, err = advance(input.State, action)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(patch)
}
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: retire-arc-pool <claim|inspect|fenced|drained|quiescing|quiesced|uninstalling|uninstalled|credential-removing|absent|restored>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ARC retirement refused:", err)
		os.Exit(1)
	}
}
