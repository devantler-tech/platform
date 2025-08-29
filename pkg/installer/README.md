# Installer Package

This package provides a standardized interface for installing and uninstalling Kubernetes components.

## Structure

- `installer.go` - Defines the `Installer` interface
- `flux/` - Flux installer implementation
- `kubectl/` - kubectl-related installer implementation

## Interface

```go
type Installer interface {
    Install() error
    Uninstall() error
}
```

## Implementations

### Flux Installer

Installs or upgrades the Flux Operator via its OCI Helm chart.

```go
import "github.com/devantler-tech/platform/pkg/installer/flux"

installer := flux.New(kubeconfig, context, timeout)
if err := installer.Install(); err != nil {
    log.Fatal(err)
}
```

### Kubectl Installer

Manages ApplySet CRDs and custom resources for kubectl-based operations.

```go
import "github.com/devantler-tech/platform/pkg/installer/kubectl"

installer := kubectl.New(kubeconfig, context, timeout)
if err := installer.Install(); err != nil {
    log.Fatal(err)
}
```

## Example Usage

See `cmd/example/main.go` for a complete example of how to use the installers together.