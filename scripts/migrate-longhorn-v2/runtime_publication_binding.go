package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const runtimeControllersRoot = "k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
const runtimeComponentReference = "../../../../bases/components/longhorn-v1-retirement-runtime"
const runtimePublicationApprovalPath = "scripts/validate-eks-ci-role-policy/approved-surface.txt"

type runtimePublicationBinding struct{ Base, Candidate, Kind string }

func runtimePublicationContext(getenv func(string) string, event []byte, head string) (runtimePublicationBinding, error) {
	refused := func() (runtimePublicationBinding, error) { return runtimePublicationBinding{}, errRuntimePublication }
	sha := regexp.MustCompile(`^[a-f0-9]{40}$`)
	positive := regexp.MustCompile(`^[1-9][0-9]*$`)
	validSHA := func(s string) bool { return sha.MatchString(s) && s != strings.Repeat("0", 40) }
	if getenv("GITHUB_ACTIONS") != "true" || getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || getenv("RUNNER_ENVIRONMENT") != "github-hosted" ||
		!positive.MatchString(getenv("GITHUB_RUN_ID")) || !positive.MatchString(getenv("GITHUB_RUN_ATTEMPT")) || !validSHA(head) || !validSHA(getenv("GITHUB_SHA")) ||
		len(event) == 0 || len(event) > 1<<20 || strictjson.Validate(event, nil) != nil {
		return refused()
	}
	ref, job := getenv("GITHUB_REF"), getenv("GITHUB_JOB")
	recovery := getenv("LONGHORN_RUNTIME_RECOVERY_SOURCE")
	workflow := getenv("GITHUB_WORKFLOW_REF")
	if getenv("GITHUB_EVENT_NAME") == "merge_group" {
		// JSON names are deliberately explicit; neither a supplied checkout
		// hash nor GITHUB_SHA alone identifies the current main base.
		var envelope struct {
			Group *struct {
				Base    string `json:"base_sha"`
				Head    string `json:"head_sha"`
				BaseRef string `json:"base_ref"`
				HeadRef string `json:"head_ref"`
			} `json:"merge_group"`
		}
		if json.Unmarshal(event, &envelope) != nil || envelope.Group == nil || !validSHA(envelope.Group.Base) || envelope.Group.Head != getenv("GITHUB_SHA") ||
			envelope.Group.BaseRef != "refs/heads/main" || envelope.Group.HeadRef != ref ||
			!regexp.MustCompile(`^refs/heads/gh-readonly-queue/main/pr-[1-9][0-9]*-[a-f0-9]{40}$`).MatchString(ref) ||
			workflow != "devantler-tech/platform/.github/workflows/ci.yaml@"+ref {
			return refused()
		}
		if job == "deploy-prod" && recovery == "" && head == envelope.Group.Head {
			return runtimePublicationBinding{Base: envelope.Group.Base, Candidate: head, Kind: "queue"}, nil
		}
		if job == "heal-prod-on-failure" && validSHA(recovery) && head == recovery {
			return runtimePublicationBinding{Base: head, Candidate: head, Kind: "recovery"}, nil
		}
		return refused()
	}
	if getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || ref != "refs/heads/main" || recovery != head || head != getenv("GITHUB_SHA") {
		return refused()
	}
	if job == "deploy-prod" && workflow == "devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main" ||
		job == "rebuild" && workflow == "devantler-tech/platform/.github/workflows/dr-rebuild.yaml@refs/heads/main" {
		return runtimePublicationBinding{Base: head, Candidate: head, Kind: "recovery"}, nil
	}
	return refused()
}

// KSail's reviewed producer packages regular source files without modifying
// them. This brackets that producer's inputs; it is not an OCI layer readback.
func runtimePublicationFingerprint(root string) (string, error) {
	hash := sha256.New()
	files := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry == nil || entry.Type()&os.ModeSymlink != 0 {
			return errRuntimePublication
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
			return errRuntimePublication
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return errRuntimePublication
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return errRuntimePublication
		}
		if _, err := fmt.Fprintf(hash, "%d:%s:%d:", len(relative), relative, len(raw)); err != nil {
			return errRuntimePublication
		}
		hash.Write(raw)
		files++
		return nil
	})
	if err != nil || files == 0 {
		return "", errRuntimePublication
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func runtimePublicationActivation(base, candidate []byte) error {
	decode := func(raw []byte) (object, error) {
		encoded, err := runtimeSingleYAML(raw)
		var result object
		if err != nil || strictjson.Validate(encoded, nil) != nil || json.Unmarshal(encoded, &result) != nil ||
			text(result, "apiVersion") != "kustomize.config.k8s.io/v1beta1" || text(result, "kind") != "Kustomization" {
			return nil, errRuntimePublication
		}
		return result, nil
	}
	previous, err := decode(base)
	if err != nil {
		return err
	}
	proposed, err := decode(candidate)
	if err != nil {
		return err
	}
	components, ok := value(proposed, "components").([]any)
	if !ok {
		return errRuntimePublication
	}
	kept, count := []any{}, 0
	for _, c := range components {
		if c == runtimeComponentReference {
			count++
		} else {
			kept = append(kept, c)
		}
	}
	if count != 1 {
		return errRuntimePublication
	}
	if _, exists := previous["components"]; !exists && len(kept) == 0 {
		delete(proposed, "components")
	} else {
		proposed["components"] = kept
	}
	if !reflect.DeepEqual(previous, proposed) {
		return errRuntimePublication
	}
	return nil
}

func runtimePublicationChangeScope(data []byte) bool {
	paths := strings.Fields(string(data))
	if len(paths) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if path != runtimeControllersRoot && path != runtimePublicationApprovalPath || seen[path] {
			return false
		}
		seen[path] = true
	}
	return seen[runtimeControllersRoot] && seen[runtimePublicationApprovalPath]
}

// The existing required authorization validator establishes the new digest
// from the full native render. This guard permits only that same release's
// ledger entry to change; it cannot authorize another resource or weaken the
// authorization validator's exact-content check.
func runtimePublicationApprovalDelta(base, candidate []byte) error {
	entry := regexp.MustCompile(`^helm\.toolkit\.fluxcd\.io/v2\|HelmRelease\|longhorn-system\|longhorn ([a-f0-9]{64})$`)
	normalize := func(raw []byte) (string, string, bool) {
		if len(raw) == 0 || len(raw) > 1<<20 {
			return "", "", false
		}
		lines, hash, count := strings.Split(string(raw), "\n"), "", 0
		for i, line := range lines {
			if match := entry.FindStringSubmatch(line); match != nil {
				count++
				hash = match[1]
				lines[i] = "<matching Longhorn authorization entry>"
			}
		}
		return strings.Join(lines, "\n"), hash, count == 1
	}
	previous, oldHash, oldOK := normalize(base)
	proposed, newHash, newOK := normalize(candidate)
	if !oldOK || !newOK || previous != proposed || oldHash == newHash {
		return errRuntimePublication
	}
	return nil
}
