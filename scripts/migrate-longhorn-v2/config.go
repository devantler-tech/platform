package main

import "errors"

func verifyDesiredLayout(raw, ephemeral object) error {
	if text(raw, "metadata", "id") != "r-longhorn" || text(ephemeral, "metadata", "id") != "EPHEMERAL" {
		return errors.New("the expected Talos volume configurations are missing")
	}
	for _, volume := range []object{raw, ephemeral} {
		if text(volume, "spec", "type") != "partition" || text(volume, "spec", "provisioning", "diskSelector", "match") != "system_disk" ||
			value(volume, "spec", "provisioning", "partitionSpec", "grow") != false || text(volume, "spec", "encryption", "provider") != "luks2" {
			return errors.New("talos has not loaded the reviewed local-disk layout")
		}
		if relative := value(volume, "spec", "provisioning", "partitionSpec", "relativeMaxSize"); relative != nil && relative != float64(0) {
			return errors.New("relative partition sizing is outside this migration")
		}
		keys, ok := value(volume, "spec", "encryption", "keys").([]any)
		if !ok || len(keys) != 1 {
			return errors.New("talos encryption keys differ from the reviewed layout")
		}
		key, ok := keys[0].(map[string]any)
		if !ok || text(key, "type") != "nodeID" || value(key, "slot") != float64(0) || value(key, "lockToState") != true {
			return errors.New("talos encryption is not locked to the existing state partition")
		}
	}
	if value(raw, "spec", "provisioning", "partitionSpec", "minSize") != float64(96*1024*1024*1024) ||
		value(raw, "spec", "provisioning", "partitionSpec", "maxSize") != float64(96*1024*1024*1024) ||
		value(raw, "spec", "provisioning", "partitionSpec", "negativeMaxSize") == true ||
		value(ephemeral, "spec", "provisioning", "partitionSpec", "minSize") != float64(40*1024*1024*1024) ||
		value(ephemeral, "spec", "provisioning", "partitionSpec", "maxSize") != float64(104*1024*1024*1024) ||
		value(ephemeral, "spec", "provisioning", "partitionSpec", "negativeMaxSize") != true {
		return errors.New("talos has not reserved the reviewed raw-disk capacity")
	}
	return nil
}
