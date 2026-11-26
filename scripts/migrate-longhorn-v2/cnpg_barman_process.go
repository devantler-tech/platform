package main

import "errors"

const (
	cnpgBarmanOperatorImage = "ghcr.io/cloudnative-pg/plugin-barman-cloud:v0.16.0@sha256:6404a416718bd7aad943c67a25a7bb20d0558b43edfeab0bf167f013ca435882"
	cnpgBarmanSidecarImage  = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.16.0@sha256:c669259e20ed1e11fad821f735c396c19d929985aa05b9544b13ecc8b5a386d0"
)

// bindBarmanOperatorProcess binds the selected sidecar release to its live manager.
func bindBarmanOperatorProcess(pod, deployment, rs, node object) (cnpgOperatorProcess, error) {
	if !recoveryJSONEqual(value(deployment, "spec", "strategy"), object{"type": "Recreate"}) {
		return cnpgOperatorProcess{}, errors.New("archive controller is not the reviewed single-process strategy")
	}
	return bindCNPGControllerProcess(pod, deployment, rs, node, cnpgControllerProfile{
		deployment: "plugin-barman-cloud", container: "barman-cloud", image: cnpgBarmanOperatorImage, replicas: 1,
		args: []any{"operator", "--server-cert=/server/tls.crt", "--server-key=/server/tls.key", "--client-cert=/client/tls.crt", "--server-address=:9090", "--leader-elect", "--log-level=info"},
		env:  []any{object{"name": "SIDECAR_IMAGE", "value": cnpgBarmanSidecarImage}}, runtimeImage: cnpgReviewedBarmanImage,
	})
}

// cnpgReviewedBarmanImage accepts the verified Linux index or its matching architecture image.
func cnpgReviewedBarmanImage(image, architecture string) bool {
	platform := map[string]string{"amd64": "sha256:44860fd00b3d294bf501c059dc2832822485aa26c2d5a29c31572ff765869000", "arm64": "sha256:04c7ebbf3ce7a129919f5cb14aadaac5385e8ab22dcef9d3fab3db345f67a39f"}[architecture]
	return platform != "" && (image == "ghcr.io/cloudnative-pg/plugin-barman-cloud@sha256:6404a416718bd7aad943c67a25a7bb20d0558b43edfeab0bf167f013ca435882" || image == "ghcr.io/cloudnative-pg/plugin-barman-cloud@"+platform)
}
