package refreshfluxghcrauth

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFixtureWorkerDefaultPreservesExplicitParallelism(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "shared workflow default", want: "workers=8 timeout=10m0s"},
		{name: "explicit serial diagnostic", args: []string{"-test.parallel=1"}, want: "workers=1 timeout=10m0s"},
		{name: "explicit worker limit", args: []string{"-test.parallel", "2"}, want: "workers=2 timeout=10m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-test.run=^TestFixtureWorkerLimitProbe$", "-test.timeout=10m"}, tc.args...)
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "PLATFORM_FIXTURE_WORKER_PROBE=1", "GOMAXPROCS=4")
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), tc.want+"\n") {
				t.Fatalf("worker policy: %v; want %q, got %s", err, tc.want, output)
			}
		})
	}
}

func TestFixtureWorkerLimitProbe(t *testing.T) {
	if os.Getenv("PLATFORM_FIXTURE_WORKER_PROBE") != "1" {
		t.Skip("subprocess observation only")
	}
	fmt.Printf("workers=%s timeout=%s\n", flag.Lookup("test.parallel").Value, flag.Lookup("test.timeout").Value)
}
