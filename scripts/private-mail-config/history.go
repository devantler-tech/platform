package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"path/filepath"
	"strings"
	"time"
)

const historyDestination = "ned@devantler.tech"

type historyConfig struct {
	dir, username, sourcePassword, destinationPassword string
	addresses                                          int
}

func historyLine(dir, name string) (string, error) {
	data, err := privateInput(dir, name, 4096)
	data = bytes.TrimSuffix(data, []byte("\n"))
	if err != nil || len(data) == 0 || bytes.ContainsAny(data, "\r\n\x00") {
		return "", errConfiguration
	}
	return string(data), nil
}

func loadHistoryInputs(dir string) (historyConfig, error) {
	v := historyConfig{dir: dir}
	var err error
	v.username, err = historyLine(dir, "source-username")
	if err != nil {
		return historyConfig{}, errConfiguration
	}
	address, err := mail.ParseAddress(v.username)
	if err != nil || address.Name != "" || address.Address != v.username || len(v.username) > 254 {
		return historyConfig{}, errConfiguration
	}
	for _, target := range []struct {
		name  string
		value *string
	}{{"source-password", &v.sourcePassword}, {"destination-password", &v.destinationPassword}} {
		*target.value, err = historyLine(dir, target.name)
		if err != nil || len(*target.value) > 1024 {
			return historyConfig{}, errConfiguration
		}
	}
	inventory, err := historyLine(dir, "addresses")
	if err != nil {
		return historyConfig{}, errConfiguration
	}
	seen := make(map[string]bool)
	for _, item := range strings.Split(inventory, ",") {
		item = strings.TrimSpace(item)
		address, err := mail.ParseAddress(item)
		key := strings.ToLower(item)
		if err != nil || address.Name != "" || address.Address != item || !strings.HasSuffix(key, "@devantler.tech") || seen[key] || len(item) > 254 {
			return historyConfig{}, errConfiguration
		}
		seen[key] = true
	}
	if !seen[historyDestination] || len(seen) > 100 {
		return historyConfig{}, errConfiguration
	}
	v.addresses = len(seen)
	return v, nil
}

func historyCommand(args []string, log io.Writer) int {
	flags := flag.NewFlagSet("email-history", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("input-dir", "", "private projected history inputs")
	check := flags.Bool("check", false, "check mounted inputs without connecting")
	confirm := flags.String("confirm", "", "explicit one-time copy confirmation")
	fail := func() int { _, _ = fmt.Fprintln(log, "MAIL_HISTORY_IMPORT=REFUSED"); return 2 }
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(*dir) || *check && *confirm != "" || !*check && *confirm != "copy-icloud-history" {
		return fail()
	}
	v, err := loadHistoryInputs(*dir)
	if err != nil {
		return fail()
	}
	if *check {
		if _, err := fmt.Fprintf(log, "MAIL_HISTORY_INPUTS=READY addresses=%d\n", v.addresses); err != nil {
			return 2
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	if err := importHistory(ctx, v, observeHistory, executeHistory, log); err != nil {
		stage := "unknown"
		switch err.Error() {
		case "source", "destination", "copy", "verification", "source-change", "readback", "reconciliation":
			stage = err.Error()
		}
		_, _ = fmt.Fprintf(log, "MAIL_HISTORY_IMPORT=INCOMPLETE stage=%s\n", stage)
		return 3
	}
	return 0
}
