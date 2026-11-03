package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	originalInventory  = "c1ed26dbc0e724b6d65694aee86f830c9532c046a8674a29e3e001d2a88242e2"
	correctedInventory = "17ebfc2e4bff742a1187de960d316d346618f49e498ef253ce51b9c2e4421379"
	quotaPolicyPath    = "policies/kubernetes/policies/use_resource_quota.rego"
)

func policyInventory(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("policy root must be a real directory")
	}
	var records []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 || info.Size() > 4<<20 {
			return fmt.Errorf("policy inventory contains an unreadable, linked, oversized or nonregular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		records = append(records, fmt.Sprintf("%s\x00%x\n", filepath.ToSlash(rel), sha256.Sum256(data)))
		if len(records) > 700 {
			return fmt.Errorf("unexpected policy inventory size")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "", fmt.Errorf("empty policy inventory")
	}
	sort.Strings(records)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "")))), nil
}

func verifyBundle(cache, want string) error {
	policy := filepath.Join(cache, "policy")
	for _, path := range []string{cache, policy} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("missing or linked native policy cache")
		}
	}
	data, err := os.ReadFile(filepath.Join(policy, "metadata.json"))
	if err != nil {
		return err
	}
	var metadata struct {
		Digest       string
		MajorVersion int
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.MajorVersion != 2 || metadata.Digest != "sha256:b591e9190cff4aa4e385b1a25bccfd9ecf178d3c77fd194e1410b7ce7e1df058" {
		return fmt.Errorf("unknown native policy bundle identity")
	}
	got, err := policyInventory(filepath.Join(policy, "content"))
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("unknown or changed complete policy inventory: %s", got)
	}
	return nil
}

func correctedBundle(from, to string) error {
	if err := verifyBundle(from, originalInventory); err != nil {
		return err
	}
	root := filepath.Join(from, "policy")
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(to, "policy", rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular policy cache input")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == "content/"+quotaPolicyPath {
			data, err = patchQuotaRule(data)
			if err != nil {
				return err
			}
		}
		return os.WriteFile(dest, data, 0400)
	}); err != nil {
		return err
	}
	if err := verifyBundle(from, originalInventory); err != nil {
		return err
	}
	return verifyBundle(to, correctedInventory)
}

func validateLoaderLog(log []byte) error {
	text := string(log)
	if !strings.Contains(text, "Checks successfully loaded from disk") {
		return fmt.Errorf("native disk policy loading was not established")
	}
	loads := regexp.MustCompile(`Checks from disk are loaded[^\n]*count=(\d+)\b`).FindAllStringSubmatch(text, -1)
	if len(loads) == 0 {
		return fmt.Errorf("native policy module coverage is missing")
	}
	for _, load := range loads {
		if load[1] != "580" {
			return fmt.Errorf("incomplete native policy module coverage")
		}
	}
	if regexp.MustCompile(`(?m)^(?:\S+\s+)?(?:ERROR|FATAL)(?:\s|$)|(?i:falling back|skip(?:ping|ped)? (?:the )?(?:rego )?(?:checks?|polic(?:y|ies))\b|(?:checks?|polic(?:y|ies))[^\n]{0,80}\bskipped\b|rego_parse_error|rego_compile_error|compile failure|failed to (?:load|parse|compile)[^\n]*(?:check|polic|rego))`).MatchString(text) {
		return fmt.Errorf("native policy fallback, omission or compiler failure")
	}
	return nil
}
