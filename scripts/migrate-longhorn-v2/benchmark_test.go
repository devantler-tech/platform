package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func fioFixture(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, profile := range []string{"randread", "randwrite", "randrw"} {
		for _, depth := range []int{1, 32} {
			for trial := 1; trial <= 3; trial++ {
				read, write := 100.0, 50.0
				if profile == "randread" {
					write = 0
				}
				if profile == "randwrite" {
					read = 0
				}
				direction := func(iops float64) object {
					return object{"iops": iops, "runtime": 30000, "io_bytes": iops * 4096 * 30,
						"clat_ns": object{"mean": 10000, "percentile": object{"99.000000": 20000}}}
				}
				job := object{"jobname": fmt.Sprintf("%s-%d-%d", profile, depth, trial), "error": 0,
					"job options": object{"rw": profile, "bs": "4k", "iodepth": fmt.Sprint(depth), "direct": "1", "ioengine": "libaio", "size": "2G", "numjobs": "1", "rwmixread": "70"},
					"read":        direction(read), "write": direction(write), "usr_cpu": 10, "sys_cpu": 5}
				if err := json.NewEncoder(&b).Encode(object{"throttled_usec": 0, "fio": object{"fio version": "fio-3.41", "jobs": []any{job}}}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return b.Bytes()
}

func TestBenchmarkRequiresAllTrialsAndSuccessfulComparableIO(t *testing.T) {
	input := fioFixture(t)
	metrics, err := parseFIO(input)
	if err != nil || len(metrics) != 18 {
		t.Fatalf("complete measurements rejected: %v", err)
	}
	for name, bad := range map[string][]byte{
		"missing trial":   input[:bytes.LastIndex(input[:len(input)-1], []byte("\n"))+1],
		"duplicate trial": append(append([]byte{}, input...), bytes.Split(input, []byte("\n"))[0]...),
		"fio error":       bytes.Replace(input, []byte(`"error":0`), []byte(`"error":5`), 1),
		"cached IO":       bytes.Replace(input, []byte(`"direct":"1"`), []byte(`"direct":"0"`), 1),
		"other engine":    bytes.Replace(input, []byte(`"ioengine":"libaio"`), []byte(`"ioengine":"sync"`), 1),
		"short sample":    bytes.Replace(input, []byte(`"runtime":30000`), []byte(`"runtime":100`), 1),
		"malformed":       append(append([]byte{}, input...), []byte("partial result")...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFIO(bad); err == nil {
				t.Fatal("incomplete, failed or incomparable benchmark accepted")
			}
		})
	}
}

func TestBenchmarkResourcesUseOnlyOwnedScratchStorageAndPermanentClient(t *testing.T) {
	objects, err := benchmarkResources("v1", "1234", "longhorn-benchmark-v1-1234")
	if err != nil {
		t.Fatal(err)
	}
	var pod, claim, class object
	for _, o := range objects {
		switch text(o, "kind") {
		case "Pod":
			pod = o
		case "PersistentVolumeClaim":
			claim = o
		case "StorageClass":
			class = o
		}
	}
	if text(pod, "spec", "nodeSelector", "kubernetes.io/hostname") != "prod-worker-3" ||
		value(pod, "spec", "automountServiceAccountToken") != false ||
		text(claim, "spec", "storageClassName") != text(class, "metadata", "name") ||
		text(class, "parameters", "dataEngine") != "v1" || text(class, "parameters", "numberOfReplicas") != "3" ||
		text(class, "reclaimPolicy") != "Retain" || text(class, "parameters", "nodeSelector") != "longhorn-benchmark-v1-1234" {
		t.Fatal("benchmark escaped its scratch storage, node or retention boundary")
	}
	if _, err := benchmarkResources("v3", "1234", "foreign"); err == nil {
		t.Fatal("unreviewed benchmark variant accepted")
	}
}
