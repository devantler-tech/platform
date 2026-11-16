// Package oauth2proxyerrorpage checks the Devantler Tech error page that
// oauth2-proxy serves, from
// k8s/bases/infrastructure/controllers/oauth2-proxy/config-map.yaml.
//
// oauth2-proxy parses its templates once, when it starts. A template that does
// not parse stops the proxy, and with it single sign-on for every app behind
// it. These tests load the template the way oauth2-proxy v7.15.6 does
// (pkg/app/pagewriter/templates.go) and render it with the data its error page
// writer passes (pkg/app/pagewriter/error_page.go) for 403, 500 and 502. They
// also check that the HelmRelease mounts the template where the proxy looks
// for it. Every check is first run against a broken copy, to prove it fails.
package oauth2proxyerrorpage

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	configMapPath     = "../../k8s/bases/infrastructure/controllers/oauth2-proxy/config-map.yaml"
	helmReleasePath   = "../../k8s/bases/infrastructure/controllers/oauth2-proxy/helm-release.yaml"
	dexThemePath      = "../../k8s/bases/infrastructure/controllers/dex/config-map.yaml"
	homepageBrandPath = "../../k8s/bases/apps/homepage/config-map-brand.yaml"

	templatesConfigMap = "oauth2-proxy-templates"
	templatesNamespace = "oauth2-proxy"
	templateFile       = "error.html"
	reloadAnnotation   = "configmap.reloader.stakater.com/reload"
	proxyPrefix        = "/oauth2"
	proxyVersion       = "v7.15.6"
)

type configMap struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

type helmRelease struct {
	Spec struct {
		Values struct {
			DeploymentAnnotations map[string]string `json:"deploymentAnnotations"`
			Config                struct {
				ConfigFile string `json:"configFile"`
			} `json:"config"`
			ExtraVolumes []struct {
				Name      string `json:"name"`
				ConfigMap *struct {
					Name  string `json:"name"`
					Items []any  `json:"items"`
				} `json:"configMap"`
			} `json:"extraVolumes"`
			ExtraVolumeMounts []struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
				SubPath   string `json:"subPath"`
				ReadOnly  bool   `json:"readOnly"`
			} `json:"extraVolumeMounts"`
		} `json:"values"`
	} `json:"spec"`
}

// errorPageData has the fields, in the order and with the types, that
// oauth2-proxy v7.15.6's WriteErrorPage passes to the error template.
type errorPageData struct {
	Title       string
	Message     string
	ProxyPrefix string
	StatusCode  int
	Redirect    string
	RequestID   string
	Footer      template.HTML
	Version     string
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// sources holds the committed files the checks read.
type sources struct {
	page    string // error.html from the oauth2-proxy-templates ConfigMap
	dexLogo string // the Dex theme's logo.svg, the page's wordmark source
	icon    string // Homepage's icon.svg, the page's tab icon source
}

func readSources(t *testing.T) sources {
	t.Helper()
	var templates, dex, homepage configMap
	readYAML(t, configMapPath, &templates)
	readYAML(t, dexThemePath, &dex)
	readYAML(t, homepageBrandPath, &homepage)
	s := sources{
		page:    templates.Data[templateFile],
		dexLogo: strings.TrimSpace(dex.Data["logo.svg"]),
		icon:    strings.TrimSpace(homepage.Data["icon.svg"]),
	}
	if s.page == "" || s.dexLogo == "" || s.icon == "" {
		t.Fatal("a source file is missing: the error template, the Dex logo.svg or Homepage's icon.svg")
	}
	return s
}

// loadErrorTemplate loads a template file the way oauth2-proxy v7.15.6's
// loadTemplates does with custom_templates_dir set: a fresh template set with
// the ToUpper and ToLower functions, the sign-in template first (a stand-in
// here, since the platform never shows that page), then ParseFiles on
// <dir>/error.html, and the page writer looks up "error.html".
func loadErrorTemplate(dir, source string) (*template.Template, error) {
	path := filepath.Join(dir, templateFile)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		return nil, err
	}
	set := template.New("").Funcs(template.FuncMap{
		"ToUpper": strings.ToUpper,
		"ToLower": strings.ToLower,
	})
	set, err := set.Parse(`{{define "sign_in.html"}}{{end}}`)
	if err != nil {
		return nil, err
	}
	set, err = set.ParseFiles(path)
	if err != nil {
		return nil, fmt.Errorf("oauth2-proxy would not start: %w", err)
	}
	page := set.Lookup(templateFile)
	if page == nil {
		return nil, errors.New("the template set has no error.html")
	}
	return page, nil
}

type errorCase struct {
	name string
	data errorPageData
}

// errorCases are the pages the platform's proxy shows, with the messages
// oauth2-proxy v7.15.6 passes for each. Footer is "-", as configured.
var errorCases = []errorCase{
	{"403-login-failed", errorPageData{
		Title: "Forbidden", StatusCode: 403, ProxyPrefix: proxyPrefix, Footer: "-", Version: proxyVersion,
		Message:   "Login Failed: Unable to find a valid CSRF token. Please try again.",
		Redirect:  "/dashboard?tab=1",
		RequestID: "3f1c0b8e-4031-4a0e-9d6f-403403403403",
	}},
	{"500-internal", errorPageData{
		Title: "Internal Server Error", StatusCode: 500, ProxyPrefix: proxyPrefix, Footer: "-", Version: proxyVersion,
		Message:   "Oops! Something went wrong. For more information contact your server administrator.",
		RequestID: "3f1c0b8e-4031-4a0e-9d6f-500500500500",
	}},
	{"502-upstream", errorPageData{
		Title: "Bad Gateway", StatusCode: 502, ProxyPrefix: proxyPrefix, Footer: "-", Version: proxyVersion,
		Message:   "There was a problem connecting to the upstream server.",
		RequestID: "3f1c0b8e-4031-4a0e-9d6f-502502502502",
	}},
}

var (
	externalReference = regexp.MustCompile(`(?i)(\b(src|href|action)\s*=\s*["']?\s*(https?:)?//|url\(\s*["']?\s*(https?:)?//|@import)`)
	faviconDataURL    = regexp.MustCompile(`<link rel="icon" type="image/svg\+xml" href="data:image/svg\+xml;base64,([A-Za-z0-9+/=]+)">`)
)

// checkPage loads and renders the template for every error case and checks
// what a visitor sees. It returns the first problem, or nil.
func checkPage(dir string, s sources) error {
	page, err := loadErrorTemplate(dir, s.page)
	if err != nil {
		return err
	}
	if !strings.Contains(s.page, s.dexLogo) {
		return errors.New("the inline wordmark is not the Dex theme's logo.svg")
	}
	icon := faviconDataURL.FindStringSubmatch(s.page)
	if icon == nil {
		return errors.New("the page has no inline SVG tab icon")
	}
	decoded, err := base64.StdEncoding.DecodeString(icon[1])
	if err != nil || strings.TrimSpace(string(decoded)) != s.icon {
		return errors.New("the tab icon is not Homepage's icon.svg")
	}
	cases := append(slices.Clone(errorCases), errorCase{"escapes-message", errorPageData{
		Title: "Forbidden", StatusCode: 403, ProxyPrefix: proxyPrefix, Footer: "-", Version: proxyVersion,
		Message: `<script>alert("x")</script>`, RequestID: "escape-check",
	}})
	for _, c := range cases {
		var out bytes.Buffer
		if err := page.Execute(&out, c.data); err != nil {
			return fmt.Errorf("%s: rendering failed, so the proxy would answer a bare 500: %w", c.name, err)
		}
		if err := checkRendered(out.String(), c.data); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}
	return nil
}

func checkRendered(html string, d errorPageData) error {
	text := template.HTMLEscapeString
	want := map[string]string{
		"the status code":   fmt.Sprintf(`<p class="status">%d</p>`, d.StatusCode),
		"the status text":   "<h1>" + text(d.Title) + "</h1>",
		"the message":       text(d.Message),
		"the request ID":    "Request ID: <code>" + text(d.RequestID) + "</code>",
		"the brand":         `aria-label="Devantler Tech"`,
		"the window title":  fmt.Sprintf("<title>%d %s · Devantler Tech</title>", d.StatusCode, text(d.Title)),
		"the document type": "<!DOCTYPE html>",
	}
	for what, fragment := range want {
		if !strings.Contains(html, fragment) {
			return fmt.Errorf("the page does not show %s (%q)", what, fragment)
		}
	}
	signIn := `<form method="GET" action="` + d.ProxyPrefix + `/sign_in">`
	if d.Redirect != "" {
		for _, fragment := range []string{
			signIn,
			`<input type="hidden" name="rd" value="` + text(d.Redirect) + `">`,
			`<form method="GET" action="` + text(d.Redirect) + `">`,
		} {
			if !strings.Contains(html, fragment) {
				return fmt.Errorf("the page does not offer to sign in again (%q)", fragment)
			}
		}
	} else if strings.Contains(html, signIn) {
		return errors.New("the page offers to sign in again without a page to return to")
	}
	if strings.Contains(html, "<script>") {
		return errors.New("the page runs a script, or an unescaped one got in through the message")
	}
	if strings.Contains(html, d.Version) || strings.Contains(html, "OAuth2 Proxy") {
		return errors.New("the page shows the proxy or its version")
	}
	if match := externalReference.FindString(html); match != "" {
		return fmt.Errorf("the page loads something from outside itself (%q)", match)
	}
	return nil
}

var (
	templatesDirLine = regexp.MustCompile(`(?m)^\s*custom_templates_dir\s*=\s*"([^"]*)"\s*$`)
	footerLine       = regexp.MustCompile(`(?m)^\s*footer\s*=\s*"-"\s*$`)
)

// checkWiring checks that the proxy is told to read its templates from the
// directory the ConfigMap is mounted at, and is restarted when it changes.
func checkWiring(release helmRelease, templates configMap) error {
	if templates.Metadata.Name != templatesConfigMap || templates.Metadata.Namespace != templatesNamespace {
		return fmt.Errorf("the template ConfigMap is %s/%s, not %s/%s",
			templates.Metadata.Namespace, templates.Metadata.Name, templatesNamespace, templatesConfigMap)
	}
	values := release.Spec.Values
	dirs := templatesDirLine.FindAllStringSubmatch(values.Config.ConfigFile, -1)
	if len(dirs) != 1 || !filepath.IsAbs(dirs[0][1]) {
		return errors.New("the config file must set custom_templates_dir to one absolute directory")
	}
	dir := filepath.Clean(dirs[0][1])
	if len(footerLine.FindAllString(values.Config.ConfigFile, -1)) != 1 {
		return errors.New(`the config file must set footer = "-", which keeps the version off the built-in page`)
	}
	volume := ""
	for _, v := range values.ExtraVolumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == templatesConfigMap {
			if len(v.ConfigMap.Items) != 0 {
				return errors.New("the template volume must map every key, without items")
			}
			volume = v.Name
		}
	}
	if volume == "" {
		return fmt.Errorf("no extra volume mounts the %s ConfigMap", templatesConfigMap)
	}
	mounted := 0
	for _, m := range values.ExtraVolumeMounts {
		if filepath.Clean(m.MountPath) != dir {
			continue
		}
		if m.Name != volume || m.SubPath != "" || !m.ReadOnly {
			return fmt.Errorf("%s must be the %s volume, read-only, as a whole directory", dir, volume)
		}
		mounted++
	}
	if mounted != 1 {
		return fmt.Errorf("the template volume is not mounted at custom_templates_dir (%s) exactly once", dir)
	}
	reload := strings.Split(values.DeploymentAnnotations[reloadAnnotation], ",")
	if !slices.Contains(reload, templatesConfigMap) {
		return fmt.Errorf("%s must name %s, or a changed template waits for the next restart", reloadAnnotation, templatesConfigMap)
	}
	return nil
}

func readWiring(t *testing.T) (helmRelease, configMap) {
	t.Helper()
	var release helmRelease
	var templates configMap
	readYAML(t, helmReleasePath, &release)
	readYAML(t, configMapPath, &templates)
	return release, templates
}

func TestErrorPage(t *testing.T) {
	if err := checkPage(t.TempDir(), readSources(t)); err != nil {
		t.Fatal(err)
	}
}

func TestErrorPageChecksFailOnABrokenCopy(t *testing.T) {
	s := readSources(t)
	for _, broken := range []struct {
		name   string
		mutate func(sources) sources
	}{
		{"does-not-parse", func(s sources) sources { s.page = strings.Replace(s.page, "{{ end }}", "", 1); return s }},
		{"unknown-field", func(s sources) sources {
			s.page = strings.Replace(s.page, "</main>", "{{.Missing}}</main>", 1)
			return s
		}},
		{"no-status-code", func(s sources) sources {
			s.page = strings.Replace(s.page, `<p class="status">{{.StatusCode}}</p>`, `<p class="status"></p>`, 1)
			return s
		}},
		{"no-request-id", func(s sources) sources {
			s.page = strings.Replace(s.page, "<code>{{.RequestID}}</code>", "", 1)
			return s
		}},
		{"no-sign-in", func(s sources) sources {
			s.page = strings.Replace(s.page, "{{.ProxyPrefix}}/sign_in", "/", 1)
			return s
		}},
		{"shows-version", func(s sources) sources {
			s.page = strings.Replace(s.page, "</main>", "{{.Version}}</main>", 1)
			return s
		}},
		{"no-message", func(s sources) sources { s.page = strings.Replace(s.page, "{{.Message}}", "", 1); return s }},
		{"external-stylesheet", func(s sources) sources {
			s.page = strings.Replace(s.page, "</head>", `<link rel="stylesheet" href="https://example.com/x.css"></head>`, 1)
			return s
		}},
		{"wordmark-drift", func(s sources) sources { s.dexLogo = strings.Replace(s.dexLogo, "#F1F5F1", "#FFFFFF", 1); return s }},
		{"icon-drift", func(s sources) sources { s.icon = strings.Replace(s.icon, "#0A0D0B", "#000000", 1); return s }},
	} {
		t.Run(broken.name, func(t *testing.T) {
			b := broken.mutate(s)
			if b == s {
				t.Fatal("the mutation changed nothing; the fixture no longer matches the template")
			}
			err := checkPage(t.TempDir(), b)
			if err == nil {
				t.Fatal("the broken copy passed")
			}
			t.Logf("failed as it should: %v", err)
		})
	}
}

func TestWiring(t *testing.T) {
	if err := checkWiring(readWiring(t)); err != nil {
		t.Fatal(err)
	}
}

func TestWiringChecksFailOnABrokenCopy(t *testing.T) {
	for _, broken := range []struct {
		name   string
		mutate func(*helmRelease, *configMap)
	}{
		{"configmap-renamed", func(_ *helmRelease, c *configMap) { c.Metadata.Name = "other" }},
		{"no-templates-dir", func(r *helmRelease, _ *configMap) {
			r.Spec.Values.Config.ConfigFile = templatesDirLine.ReplaceAllString(r.Spec.Values.Config.ConfigFile, "")
		}},
		{"templates-dir-elsewhere", func(r *helmRelease, _ *configMap) {
			r.Spec.Values.Config.ConfigFile = templatesDirLine.ReplaceAllString(r.Spec.Values.Config.ConfigFile, `custom_templates_dir = "/templates"`)
		}},
		{"no-footer", func(r *helmRelease, _ *configMap) {
			r.Spec.Values.Config.ConfigFile = footerLine.ReplaceAllString(r.Spec.Values.Config.ConfigFile, "")
		}},
		{"volume-from-other-configmap", func(r *helmRelease, _ *configMap) {
			for i := range r.Spec.Values.ExtraVolumes {
				if v := r.Spec.Values.ExtraVolumes[i].ConfigMap; v != nil {
					v.Name = "other"
				}
			}
		}},
		{"mounted-elsewhere", func(r *helmRelease, _ *configMap) {
			for i := range r.Spec.Values.ExtraVolumeMounts {
				r.Spec.Values.ExtraVolumeMounts[i].MountPath = "/elsewhere"
			}
		}},
		{"mounted-with-subpath", func(r *helmRelease, _ *configMap) {
			for i := range r.Spec.Values.ExtraVolumeMounts {
				r.Spec.Values.ExtraVolumeMounts[i].SubPath = templateFile
			}
		}},
		{"not-reloaded", func(r *helmRelease, _ *configMap) {
			r.Spec.Values.DeploymentAnnotations[reloadAnnotation] = "oauth2-proxy"
		}},
	} {
		t.Run(broken.name, func(t *testing.T) {
			release, templates := readWiring(t)
			broken.mutate(&release, &templates)
			err := checkWiring(release, templates)
			if err == nil {
				t.Fatal("the broken copy passed")
			}
			t.Logf("failed as it should: %v", err)
		})
	}
}
