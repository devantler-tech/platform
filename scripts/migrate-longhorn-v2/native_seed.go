package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const nativeSeedOperation = "prepare-native-v1-seed"
const nativeSeedJournal = "longhorn-v2-native-seed"
const nativeSeedConfirmation = "prepare-longhorn-native-v1-seed"

const nativeSeedScript = `set -eu
while [ ! -f /tmp/canary-start ]; do sleep 2; done
fio --name=longhorn-v2-canary --filename=/data/payload.bin --size=128M --rw=write --bs=4k --ioengine=libaio --direct=1 --verify=sha256 --do_verify=1 --verify_fatal=1 --verify_state_save=0 --end_fsync=1 --output=/tmp/probe.json --output-format=json
cd /data
sha256sum payload.bin > payload.sha256
printf 'longhorn-native-canary-5582\n' > sentinel
sync
printf 'CANARY_READY\n'
while :; do sleep 10; done
`

const nativeSeedVerify = `set -eu
cd /data
test "$(cat sentinel)" = longhorn-native-canary-5582
test "$(wc -c < payload.bin)" -eq 134217728
sha256sum --check payload.sha256 >/dev/null
sha256sum payload.bin
`

func nativeSeedWorkload() string {
	digest := sha256.Sum256([]byte(nativeSeedScript + nativeSeedVerify))
	return hex.EncodeToString(digest[:])
}

func authorizeNativeSeed(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != nativeSeedOperation || getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return errors.New("only the separately confirmed disposable native V1 seed operation is authorized")
	}
	return authorizeWorkflow(getenv, nativeSeedConfirmation)
}

func authorizeScratchV1(c client, getenv func(string) string) error {
	if c.nativeSeed {
		return authorizeNativeSeed(getenv)
	}
	return authorizeBenchmark(getenv, "v1")
}

func benchmarkScratchAssets(c client, phase, run, namespace string) ([]object, error) {
	if c.nativeSeed && (phase != "v1" || namespace != benchmarkNamespace) {
		return nil, errors.New("the native seed is outside its fixed V1 workspace")
	}
	assets, err := benchmarkResources(phase, run, namespace)
	if err != nil || !c.nativeSeed {
		return assets, err
	}
	for _, asset := range assets {
		kind := text(asset, "kind")
		if kind == "Pod" || kind == "PersistentVolumeClaim" {
			delete(value(asset, "metadata", "labels").(object), "velero.io/exclude-from-backup")
		}
		if text(asset, "kind") == "Pod" {
			value(asset, "metadata").(object)["annotations"] = object{"backup.velero.io/backup-volumes": "data"}
			value(asset, "spec", "containers").([]any)[0].(object)["args"] = []any{nativeSeedScript}
			value(asset, "spec").(object)["activeDeadlineSeconds"] = 2700
		}
	}
	return assets, nil
}
