package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// v2ManagerImage is qualified against the reviewed Longhorn 1.13.0 chart;
// matching unreviewed live declarations cannot establish image provenance.
const v2ManagerImage = "longhornio/longhorn-instance-manager:v1.13.0"

// verifyV2Settings binds coexistence and effective SPDK configuration to the
// reviewed profile before either registration or subsequent storage acceptance.
func verifyV2Settings(ctx context.Context, c client) error {
	for name, expected := range map[string]string{
		"v1-data-engine": "true", "v2-data-engine": "true", "guaranteed-instance-manager-cpu": `{"v1":"6","v2":"25"}`,
		"data-engine-cpu-mask": `{"v2":"0x3"}`, "data-engine-number-of-cpu-cores": `{"v2":"0"}`, "data-engine-cpu-isolation-enabled": `{"v2":"true"}`,
		"data-engine-interrupt-mode-enabled": `{"v2":"false"}`, "data-engine-hugepage-enabled": `{"v2":"true"}`, "data-engine-memory-size": `{"v2":"2048"}`,
	} {
		o, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", name, false)
		if err != nil {
			return err
		}
		actual := text(o, "value")
		if strings.HasPrefix(expected, "{") {
			var want, got map[string]string
			if json.Unmarshal([]byte(expected), &want) != nil || json.Unmarshal([]byte(actual), &got) != nil || !reflect.DeepEqual(want, got) {
				return errors.New("live V2 settings have not adopted the reviewed configuration")
			}
		} else if actual != expected {
			return errors.New("both engines must remain enabled during storage qualification")
		}
	}
	return nil
}

// v2Environment proves effective worker clients and three independent ready
// raw disks. Merely enabling the V2 setting cannot establish storage readiness.
func v2Environment(ctx context.Context, c client, pools map[string]migrationPool) (inventory, error) {
	if err := verifyV2Settings(ctx, c); err != nil {
		return inventory{}, err
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return inventory{}, err
	}
	managers, err := c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil {
		return inventory{}, err
	}
	if err = verifyV2Runtime(s, managers, pools, v2ManagerImage); err != nil {
		return inventory{}, err
	}
	return s, nil
}
