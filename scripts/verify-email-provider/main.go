// verify-email-provider performs only the fixed, guarded hosting-network proof.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

func main() {
	valid := (len(os.Args) == 1 || (len(os.Args) == 2 && os.Args[1] == "--cleanup")) && os.Getenv("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		os.Getenv("GITHUB_ACTOR") == "devantler" && os.Getenv("GITHUB_REF") == "refs/heads/main" &&
		os.Getenv("GITHUB_EVENT_NAME") == "workflow_dispatch" && os.Getenv("GITHUB_RUN_ATTEMPT") == "1" &&
		os.Getenv("EMAIL_PROVIDER_CONFIRM") == "verify-email-provider-smtp" &&
		regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(os.Getenv("GITHUB_RUN_ID")) &&
		regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(os.Getenv("GITHUB_SHA"))
	if !valid {
		fmt.Println("EMAIL_PROVIDER_SMTP=UNVERIFIED stage=dispatch")
		os.Exit(2)
	}
	run, sha := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_SHA")
	temp := os.Getenv("RUNNER_TEMP")
	if !filepath.IsAbs(temp) || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		fmt.Println("EMAIL_PROVIDER_SMTP=UNVERIFIED stage=configuration")
		os.Exit(3)
	}
	r := runner{k: kubectl{}, run: run, sha: sha, receipt: filepath.Join(temp, "email-provider-"+run+".json"), pause: pause}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	var err error
	if len(os.Args) == 2 {
		err = r.restore()
		if err == nil {
			err = r.cleanup(ctx)
		}
	} else {
		r.expectedImage, err = sourceImage()
		if err == nil {
			err = r.execute(ctx)
		}
	}
	if err != nil {
		// Errors are deliberately fixed categories, never raw API responses.
		fmt.Println("EMAIL_PROVIDER_SMTP=UNVERIFIED stage=" + err.Error())
		os.Exit(3)
	}
	if len(os.Args) == 2 {
		fmt.Println("EMAIL_PROVIDER_CLEANUP=VERIFIED")
		return
	}
	fmt.Println("EMAIL_PROVIDER_SMTP=VERIFIED scope=hosting-node-no-message cleanup=verified")
}

type kubectl struct{}

func (kubectl) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=20s"}, args...)...)
	cmd.Stdin = strings.NewReader(string(input))
	// Output is examined privately. Stderr can contain topology and is discarded.
	return cmd.Output()
}

func sourceImage() (string, error) {
	data, err := os.ReadFile("k8s/bases/apps/private-mail-administration/kustomization.yaml")
	if err != nil {
		return "", fmt.Errorf("source")
	}
	var config struct {
		Images []struct{ Name, Digest string }
	}
	if yaml.Unmarshal(data, &config) != nil {
		return "", fmt.Errorf("source")
	}
	const name = "ghcr.io/devantler-tech/platform-private-mail-bootstrap"
	var images []string
	for _, image := range config.Images {
		if image.Name == name && regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(image.Digest) {
			images = append(images, name+"@"+image.Digest)
		}
	}
	if len(images) != 1 {
		return "", fmt.Errorf("source")
	}
	return images[0], nil
}

func pause(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("deadline")
	case <-time.After(duration):
		return nil
	}
}

// Keep the transport wrapper's JSON encoding available to the lifecycle driver.
func marshal(value any) []byte { data, _ := json.Marshal(value); return data }
