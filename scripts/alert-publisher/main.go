// alert-publisher is deliberately not wired to a deployment or existing route.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type remoteFactory func(int64, []byte) (githubAPI, error)

func command(args []string, input io.Reader, output io.Writer, factory remoteFactory) int {
	flags := flag.NewFlagSet("alert-publisher", flag.ContinueOnError)
	// Flag parse failures can echo untrusted argument values. Emit fixed errors.
	flags.SetOutput(io.Discard)
	enabled := flags.Bool("enabled", false, "enable the prepared publisher")
	drain := flags.Bool("drain", false, "process pending journal entries only")
	dir := flags.String("state-dir", "", "private durable state directory")
	keyPath := flags.String("key-file", "", "projected App private key file")
	installation := flags.Int64("installation-id", 0, "configured existing App installation")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		fmt.Fprintln(output, "Invalid publisher invocation.")
		return 2
	}
	if !*enabled {
		fmt.Fprintln(output, "Publisher is disabled.")
		return 0
	}
	if !filepath.IsAbs(*dir) || !filepath.IsAbs(*keyPath) || *installation <= 0 {
		fmt.Fprintln(output, "Publisher deployment facts are incomplete.")
		return 2
	}
	var event *observation
	if !*drain {
		value, err := decodeObservation(input)
		if err != nil {
			fmt.Fprintln(output, errInput)
			return 2
		}
		event = &value
	}
	keyFile, err := os.Open(*keyPath)
	if err != nil {
		fmt.Fprintln(output, "Publisher credential is unavailable.")
		return 2
	}
	info, statErr := keyFile.Stat()
	key, readErr := io.ReadAll(io.LimitReader(keyFile, 64*1024+1))
	closeErr := keyFile.Close()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0027 != 0 || readErr != nil || closeErr != nil || len(key) == 0 || len(key) > 64*1024 {
		fmt.Fprintln(output, "Publisher credential is unavailable.")
		return 2
	}
	api, err := factory(*installation, key)
	if err != nil {
		fmt.Fprintln(output, "Publisher App identity is unverified.")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	p := publisher{dir: *dir, api: api, now: time.Now}
	if err = p.run(ctx, event); err != nil {
		fmt.Fprintln(output, err)
		if errors.Is(err, errQuarantined) {
			return 4
		}
		if errors.Is(err, errStore) || errors.Is(err, errFull) || errors.Is(err, errInput) {
			return 2
		}
		return 3
	}
	fmt.Fprintln(output, "Observation processed; confirmed receipts are recorded in the journal.")
	return 0
}
func main() {
	os.Exit(command(os.Args[1:], os.Stdin, os.Stderr, func(id int64, key []byte) (githubAPI, error) { return newGitHubRemote(id, key) }))
}
