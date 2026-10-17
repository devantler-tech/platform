// Compare the installed Talos v1.13 rule decisions with their declared policy.
// Native resource schema: https://github.com/siderolabs/talos/blob/v1.13.10/pkg/machinery/resources/security/image_verification_rule.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type keyless struct {
	Issuer       string `yaml:"issuer" json:"issuer"`
	Subject      string `yaml:"subject,omitempty" json:"subject"`
	SubjectRegex string `yaml:"subjectRegex,omitempty" json:"subjectRegex"`
}

type publicKey struct {
	Certificate string `yaml:"certificate" json:"certificate"`
}

type rule struct {
	ImagePattern string     `json:"imagePattern"`
	Skip         bool       `json:"skip"`
	Deny         bool       `json:"deny"`
	Keyless      *keyless   `json:"keylessVerifier"`
	PublicKey    *publicKey `json:"publicKeyVerifier"`
}

type declaredRule struct {
	Image     string     `yaml:"image"`
	Skip      bool       `yaml:"skip"`
	Deny      bool       `yaml:"deny"`
	Keyless   *keyless   `yaml:"keyless"`
	PublicKey *publicKey `yaml:"publicKey"`
}

type installedRule struct {
	ImagePattern *string    `json:"imagePattern"`
	Skip         *bool      `json:"skip"`
	Deny         *bool      `json:"deny"`
	Keyless      *keyless   `json:"keylessVerifier"`
	PublicKey    *publicKey `json:"publicKeyVerifier"`
}

func validRule(r rule) bool {
	if strings.TrimSpace(r.ImagePattern) == "" {
		return false
	}
	if r.Keyless != nil {
		if strings.TrimSpace(r.Keyless.Issuer) == "" || (r.Keyless.Subject == "" && r.Keyless.SubjectRegex == "") {
			return false
		}
		if r.Keyless.SubjectRegex != "" {
			if _, err := regexp.Compile(r.Keyless.SubjectRegex); err != nil {
				return false
			}
		}
	}
	if r.PublicKey != nil && strings.TrimSpace(r.PublicKey.Certificate) == "" {
		return false
	}
	return r.Skip || r.Deny || r.Keyless != nil || r.PublicKey != nil
}

func parsePolicy(data []byte) ([]rule, error) {
	var policy struct {
		APIVersion string         `yaml:"apiVersion"`
		Kind       string         `yaml:"kind"`
		Rules      []declaredRule `yaml:"rules"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("invalid declared policy")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one declared policy document")
	}
	if policy.APIVersion != "v1alpha1" || policy.Kind != "ImageVerificationConfig" || len(policy.Rules) == 0 {
		return nil, fmt.Errorf("missing declared verification policy")
	}
	rules := make([]rule, 0, len(policy.Rules))
	for _, r := range policy.Rules {
		value := rule{r.Image, r.Skip, r.Deny, r.Keyless, r.PublicKey}
		if !validRule(value) {
			return nil, fmt.Errorf("incomplete declared verification decision")
		}
		rules = append(rules, value)
	}
	return rules, nil
}

func parseInstalled(input io.Reader) ([]rule, error) {
	var installed []installedRule
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installed); err != nil {
		return nil, fmt.Errorf("invalid installed rule data")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing installed rule data")
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("missing installed rule data")
	}
	rules := make([]rule, 0, len(installed))
	for _, r := range installed {
		// Talos' native skip and deny fields have no omitempty tag. Their
		// absence, null or a wrong type must never normalize to healthy false.
		if r.ImagePattern == nil || r.Skip == nil || r.Deny == nil {
			return nil, fmt.Errorf("incomplete installed rule data")
		}
		value := rule{*r.ImagePattern, *r.Skip, *r.Deny, r.Keyless, r.PublicKey}
		if !validRule(value) {
			return nil, fmt.Errorf("incomplete installed verification decision")
		}
		rules = append(rules, value)
	}
	return rules, nil
}

func compare(policy string, input io.Reader, _ io.Writer, diagnostic io.Writer) int {
	expected, err := parsePolicy([]byte(policy))
	if err != nil {
		fmt.Fprintln(diagnostic, "could not parse declared verification policy")
		return 2
	}
	actual, err := parseInstalled(input)
	if err != nil {
		fmt.Fprintln(diagnostic, "could not parse complete installed verification decisions")
		return 2
	}
	if !reflect.DeepEqual(expected, actual) {
		// Do not include actual node specs, issuers, subjects or certificates.
		fmt.Fprintln(diagnostic, "installed verification decisions differ from declared policy")
		return 1
	}
	return 0
}

func run(args []string, input io.Reader, output, diagnostic io.Writer) int {
	if len(args) != 2 || (args[0] != "normalize" && args[0] != "compare") {
		fmt.Fprintln(diagnostic, "usage: validate-image-verification-policy <normalize|compare> <policy.yaml>")
		return 2
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(diagnostic, "could not read declared verification policy")
		return 2
	}
	if args[0] == "compare" {
		return compare(string(data), input, output, diagnostic)
	}
	rules, err := parsePolicy(data)
	if err != nil {
		fmt.Fprintln(diagnostic, "could not parse declared verification policy")
		return 2
	}
	if err := json.NewEncoder(output).Encode(rules); err != nil {
		return 2
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
