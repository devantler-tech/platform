# Components

This directory contains the Kustomize components used for various Kubernetes resources. Components are a way to modularize and reuse Kubernetes configuration, making it easier to manage and share common configurations across different environments or applications.

To use a component in a kustomize kustomization, add a `components` field to the kustomization file, and list the components that you want to include (relative to the current directory).

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
  - helm-release.yaml
  - helm-repository.yaml

components:
  - ../components/auth-oidc
  - ../components/ingress-traefik
```
