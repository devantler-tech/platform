// Negative control for the Go vulnerability scan. The old Go release is the
// point: its standard library has published advisories that main.go reaches,
// so the scan must report them. Never build or ship this module.
module github.com/devantler-tech/platform/scripts/tests/fixtures/govulncheck-negative-control

go 1.24.0
