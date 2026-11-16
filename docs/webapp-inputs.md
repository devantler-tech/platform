# WebApp file inputs

A WebApp may select one Secret and one ConfigMap from its own application
namespace. Each is optional in the declaration; when selected, the object and
each selected key are required at startup. Omitting both projections adds no
volume or mount and preserves the executable's normal startup.

Use the ConfigMap for release-owned public application settings and the Secret
for platform-owned private settings and credentials. The application controls
which public settings it accepts. Selecting a ConfigMap adds no authority to
publish private policy or credentials there.

```yaml
apiVersion: kro.run/v1alpha1
kind: WebApp
metadata:
  name: example-app
  namespace: example-app
spec:
  name: example-app
  image: example.invalid/example-app:v1
  host: example.invalid
  secretProjections:
    - secretName: example-app-private
      items:
        - key: private-settings
          path: private-settings
  configMapProjections:
    - configMapName: example-app-public
      items:
        - key: settings.env
          path: portal-settings
```

The process reads the selected private file at
`/run/webapp-inputs/private-settings` and the public file at
`/run/webapp-settings/portal-settings`. Both directories are read-only mounts,
with required inputs, fixed 0440 files and the process's primary group. Reloader
rolls startup-only consumers when either selected input changes. The application
must explicitly read its file; the platform does not inject environment variables.

Each list admits zero or one projection. A selected Secret has one to twelve
items; a selected ConfigMap has one to eight. Keys and destination filenames
must be unique within each projection. Keys use only letters,
digits, hyphens, periods and underscores, with a maximum of 253 characters.
Destination filenames must start with a letter or digit, contain only those
same characters, and have at most 64 characters. Namespace overrides, paths,
file-mode overrides, optional missing inputs and subPath mounts are unavailable.
The existing WebApp namespace/parent-ownership admission rules still apply.

Before publishing a WebApp, run the closed source validator on its complete
JSON form:

```sh
yq -o=json '.' webapp.yaml | bash scripts/validate-webapp-inputs.sh
```

The real generated API and compiled workload are covered by
`scripts/tests/test-webapp-compiler.sh`; source controls by
`scripts/tests/test-webapp-inputs.sh`; and controller convergence plus actual
kubelet projection permissions by the existing GitHub-hosted runtime trial.
Local maintenance validation starts no cluster. A source release may use this
field only after the supporting platform graph and generated API have deployed.
