// Synthetic native Hive formatting fixture; no agent or cluster connection.
package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"

	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
)

func main() {
	h := hive.New(cell.Config(cmtypes.PolicyConfig{PolicyDefaultLocalCluster: true}))
	var output bytes.Buffer
	if err := h.PrintObjects(&output, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		panic(err)
	}
	lines := strings.Split(output.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if _, err := io.WriteString(os.Stdout, strings.Join(lines, "\n")+"\n"); err != nil {
		panic(err)
	}
}
