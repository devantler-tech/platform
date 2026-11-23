package main

import (
	"context"
	"errors"
	"maps"
	"strings"
)

func (p *nativeSeedRun) bind(ctx context.Context) error {
	err := waitReady(ctx, func(ctx context.Context) error {
		pod, err := p.c.get(ctx, "pod", benchmarkNamespace, "fio", false)
		if err != nil || verifyNativeSeedPod(pod, p.record.Resources["Pod"].UID) != nil {
			return errSeedEvidence
		}
		claim, err := p.c.get(ctx, "persistentvolumeclaim", benchmarkNamespace, "fio-data", false)
		if err != nil || benchmarkIdentity(claim) != p.record.Resources["PersistentVolumeClaim"] || text(claim, "status", "phase") != "Bound" ||
			text(claim, "spec", "storageClassName") != p.record.Resources["StorageClass"].Name {
			return errSeedEvidence
		}
		pv, err := p.c.get(ctx, "persistentvolume", "", text(claim, "spec", "volumeName"), false)
		if err != nil {
			return err
		}
		volume, err := p.c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(pv, "spec", "csi", "volumeHandle"), false)
		if err != nil || text(volume, "spec", "dataEngine") != "v1" || text(volume, "status", "state") != "attached" || text(volume, "status", "robustness") != "healthy" ||
			verifyScratchBinding(pv, volume, claim, p.created["StorageClass"]) != nil {
			return errSeedEvidence
		}
		bound := p.record
		bound.Resources = maps.Clone(p.record.Resources)
		bound.Resources["PersistentVolume"], bound.Resources["LonghornVolume"] = benchmarkIdentity(pv), benchmarkIdentity(volume)
		class, err := p.c.get(ctx, "storageclass", "", bound.Resources["StorageClass"].Name, false)
		if err != nil {
			return err
		}
		if err = verifyNativeSeedStorage(bound, claim, class, pv, volume); err != nil {
			return err
		}
		s, err := p.c.snapshot(ctx)
		if err != nil {
			return err
		}
		if _, err = benchmarkMachines(s, "v1"); err != nil {
			return err
		}
		p.claim, p.pv, p.volume = claim, pv, volume
		return nil
	})
	if err != nil {
		return err
	}
	p.record.Resources["PersistentVolume"] = benchmarkIdentity(p.pv)
	p.record.Resources["LonghornVolume"] = benchmarkIdentity(p.volume)
	return saveBenchmarkProgress(ctx, p.c, p.record.Source, p.record.Run, p.record)
}

func (p *nativeSeedRun) originalPod(ctx context.Context) (object, error) {
	pod, err := p.c.get(ctx, "pod", benchmarkNamespace, "fio", false)
	if err != nil || verifyNativeSeedPod(pod, p.record.Resources["Pod"].UID) != nil {
		return nil, errSeedEvidence
	}
	return pod, nil
}

func (p *nativeSeedRun) verifyPayload(ctx context.Context) (string, error) {
	if err := p.originalBinding(ctx); err != nil {
		return "", err
	}
	if _, err := p.originalPod(ctx); err != nil {
		return "", err
	}
	if err := sourceCurrent(ctx, p.c.run, p.record.Source); err != nil {
		return "", err
	}
	output, err := p.c.kubectl(ctx, nil, "exec", "--namespace="+benchmarkNamespace, "fio", "--container=fio", "--", "/bin/sh", "-ec", nativeSeedVerify)
	if err != nil {
		return "", errors.New("the source payload checksum, size or sentinel was not verified")
	}
	return decodeSeedPayload(string(output))
}

func (p *nativeSeedRun) writePayload(ctx context.Context) error {
	after := p.state
	after.Phase = "source-write-requested"
	if err := p.advance(ctx, after); err != nil {
		return err
	}
	if err := p.originalBinding(ctx); err != nil {
		return err
	}
	if _, err := p.originalPod(ctx); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, p.c.run, p.record.Source); err != nil {
		return err
	}
	if _, err := p.c.kubectl(ctx, nil, "exec", "--namespace="+benchmarkNamespace, "fio", "--container=fio", "--", "/bin/sh", "-ec", "touch /tmp/canary-start"); err != nil {
		return errors.New("the source write response was not confirmed; preserve its journal")
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		if _, err := p.originalPod(ctx); err != nil {
			return err
		}
		logs, err := p.c.kubectl(ctx, nil, "logs", "--namespace="+benchmarkNamespace, "fio", "--container=fio")
		if err != nil || strings.TrimSpace(string(logs)) != "CANARY_READY" {
			return errSeedEvidence
		}
		return nil
	}); err != nil {
		return err
	}
	digest, err := p.verifyPayload(ctx)
	if err != nil {
		return err
	}
	after = p.state
	after.Phase, after.PayloadSHA256 = "payload-verified", digest
	return p.advance(ctx, after)
}

func (p *nativeSeedRun) originalBinding(ctx context.Context) error {
	objects := map[string]object{}
	for _, kind := range []string{"PersistentVolumeClaim", "StorageClass", "PersistentVolume", "LonghornVolume"} {
		identity := p.record.Resources[kind]
		query := strings.ToLower(kind)
		if kind == "LonghornVolume" {
			query = "volumes.longhorn.io"
		}
		actual, err := p.c.get(ctx, query, identity.Namespace, identity.Name, false)
		if err != nil {
			return err
		}
		objects[kind] = actual
	}
	claim, class, pv, volume := objects["PersistentVolumeClaim"], objects["StorageClass"], objects["PersistentVolume"], objects["LonghornVolume"]
	if text(claim, "status", "phase") != "Bound" || text(volume, "status", "state") != "attached" || text(volume, "status", "robustness") != "healthy" ||
		verifyNativeSeedStorage(p.record, claim, class, pv, volume) != nil {
		return errSeedEvidence
	}
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = verifyRedundancy(s); err != nil {
		return err
	}
	p.claim, p.pv, p.volume = claim, pv, volume
	return nil
}
