package main

import (
	"context"
	"errors"
	"strconv"
)

func verifyBudgetSettings(ctx context.Context, c client) error {
	for name, expected := range map[string]string{"storage-over-provisioning-percentage": "100", "storage-minimal-available-percentage": "25"} {
		setting, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", name, false)
		if err != nil || text(setting, "value") != expected {
			return errors.New("live storage scheduling settings differ from the reviewed capacity budget")
		}
	}
	return nil
}

func verifyRebuildBudget(target string, s inventory) error {
	const gib = uint64(1024 * 1024 * 1024)
	var logical, actual uint64
	if len(s.volumes) == 0 {
		return errors.New("complete storage capacity evidence is required")
	}
	for _, v := range s.volumes {
		if target == "prod-worker-3" && text(v, "spec", "dataEngine") != "v2" {
			return errors.New("the last V1 worker cannot be rebuilt while any V1 volume remains")
		}
		size, err := strconv.ParseUint(text(v, "spec", "size"), 10, 64)
		if err != nil || size == 0 || size > 96*gib {
			return errors.New("volume logical size is unknown or outside the raw-disk budget")
		}
		used, err := strconv.ParseUint(text(v, "status", "actualSize"), 10, 64)
		if err != nil || used > 96*gib {
			return errors.New("volume actual size is unknown or outside the raw-disk budget")
		}
		logical += size
		actual += used
		// 96 GiB per target, 4 GiB disk reserve, 2 GiB metadata margin,
		// and one 4 GiB disposable canary or benchmark at a time. Actual data
		// must additionally leave the current 25% minimum free-space margin.
		if logical > (96-4-2-4)*gib || actual > (96*75/100-4-2-4)*gib {
			return errors.New("current volumes leave insufficient raw-disk and canary headroom")
		}
	}
	return nil
}
