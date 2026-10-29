package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

type fioMeasurement struct {
	Profile               string
	Depth, Trial          int
	ReadIOPS, WriteIOPS   float64
	ReadP99NS, WriteP99NS float64
	UserCPU, SystemCPU    float64
	ThrottledMicroseconds int64
}

func finiteNonnegative(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func validateMeasurements(measurements []fioMeasurement) error {
	if len(measurements) != 18 {
		return errors.New("all eighteen measurements are required")
	}
	seen := map[string]bool{}
	for _, m := range measurements {
		key := fmt.Sprintf("%s-%d-%d", m.Profile, m.Depth, m.Trial)
		if seen[key] || (m.Profile != "randread" && m.Profile != "randwrite" && m.Profile != "randrw") || (m.Depth != 1 && m.Depth != 32) || m.Trial < 1 || m.Trial > 3 ||
			!finiteNonnegative(m.ReadIOPS) || !finiteNonnegative(m.WriteIOPS) || !finiteNonnegative(m.UserCPU) || !finiteNonnegative(m.SystemCPU) || m.ThrottledMicroseconds < 0 ||
			m.Profile != "randwrite" && (m.ReadIOPS <= 0 || !finiteNonnegative(m.ReadP99NS) || m.ReadP99NS <= 0) ||
			m.Profile != "randread" && (m.WriteIOPS <= 0 || !finiteNonnegative(m.WriteP99NS) || m.WriteP99NS <= 0) {
			return errors.New("benchmark measurement is invalid or duplicated")
		}
		seen[key] = true
	}
	return nil
}

func parseFIO(raw []byte) ([]fioMeasurement, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	seen := map[string]bool{}
	var result []fioMeasurement
	for {
		var entry struct {
			Throttled *int64 `json:"throttled_usec"`
			FIO       struct {
				Version string `json:"fio version"`
				Jobs    []struct {
					Name        string            `json:"jobname"`
					Error       *int              `json:"error"`
					Options     map[string]string `json:"job options"`
					Read, Write struct {
						IOPS    float64 `json:"iops"`
						Runtime int64   `json:"runtime"`
						Bytes   int64   `json:"io_bytes"`
						Latency struct {
							Percentile map[string]float64 `json:"percentile"`
						} `json:"clat_ns"`
					}
					UserCPU   float64 `json:"usr_cpu"`
					SystemCPU float64 `json:"sys_cpu"`
				} `json:"jobs"`
			} `json:"fio"`
		}
		if err := decoder.Decode(&entry); err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.New("fio output is incomplete or malformed")
		}
		if entry.Throttled == nil || *entry.Throttled < 0 || entry.FIO.Version != "fio-3.41" || len(entry.FIO.Jobs) != 1 {
			return nil, errors.New("fio execution identity is incomplete")
		}
		job := entry.FIO.Jobs[0]
		m := fioMeasurement{UserCPU: job.UserCPU, SystemCPU: job.SystemCPU, ThrottledMicroseconds: *entry.Throttled}
		if job.Error == nil || *job.Error != 0 || !finiteNonnegative(m.UserCPU) || !finiteNonnegative(m.SystemCPU) {
			return nil, errors.New("fio reported an error or invalid resource measurements")
		}
		for _, profile := range []string{"randread", "randwrite", "randrw"} {
			for _, depth := range []int{1, 32} {
				for trial := 1; trial <= 3; trial++ {
					if job.Name == fmt.Sprintf("%s-%d-%d", profile, depth, trial) {
						m.Profile, m.Depth, m.Trial = profile, depth, trial
					}
				}
			}
		}
		if m.Profile == "" || seen[job.Name] {
			return nil, errors.New("fio trial is unknown or duplicated")
		}
		for key, want := range map[string]string{"rw": m.Profile, "bs": "4k", "iodepth": fmt.Sprint(m.Depth), "direct": "1", "ioengine": "libaio", "size": "2G", "numjobs": "1", "rwmixread": "70"} {
			if job.Options[key] != want {
				return nil, errors.New("fio workload differs from the comparison contract")
			}
		}
		m.ReadIOPS, m.WriteIOPS = job.Read.IOPS, job.Write.IOPS
		m.ReadP99NS, m.WriteP99NS = job.Read.Latency.Percentile["99.000000"], job.Write.Latency.Percentile["99.000000"]
		if !finiteNonnegative(m.ReadIOPS) || !finiteNonnegative(m.WriteIOPS) {
			return nil, errors.New("fio IOPS are invalid")
		}
		if m.Profile != "randwrite" && (m.ReadIOPS <= 0 || job.Read.Bytes <= 0 || job.Read.Runtime < 29000 || !finiteNonnegative(m.ReadP99NS) || m.ReadP99NS <= 0) ||
			m.Profile != "randread" && (m.WriteIOPS <= 0 || job.Write.Bytes <= 0 || job.Write.Runtime < 29000 || !finiteNonnegative(m.WriteP99NS) || m.WriteP99NS <= 0) {
			return nil, errors.New("fio trial has no complete timed I/O measurement")
		}
		seen[job.Name] = true
		result = append(result, m)
	}
	if len(result) != 18 {
		return nil, errors.New("all eighteen fio trials are required")
	}
	return result, nil
}
