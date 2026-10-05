package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strings"
)

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([a-zA-Z]*)$`)

func quantity(value string) (*big.Rat, error) {
	if value == "" {
		return new(big.Rat), nil
	}
	parts := quantityPattern.FindStringSubmatch(value)
	if parts == nil {
		return nil, errors.New("unknown resource quantity")
	}
	number, ok := new(big.Rat).SetString(parts[1])
	if !ok {
		return nil, errors.New("invalid resource quantity")
	}
	scale := map[string]string{"": "1", "n": "0.000000001", "u": "0.000001", "m": "0.001",
		"k": "1000", "M": "1000000", "G": "1000000000", "T": "1000000000000",
		"Ki": "1024", "Mi": "1048576", "Gi": "1073741824", "Ti": "1099511627776"}
	factor, ok := scale[parts[2]]
	if !ok {
		return nil, errors.New("unsupported resource unit")
	}
	ratio, _ := new(big.Rat).SetString(factor)
	return number.Mul(number, ratio), nil
}

// Sum main, init and overhead reservations conservatively. Serial init requests
// are deliberately added rather than subtracted, preserving node headroom.
func verifyBudget(nodeJSON, reservations []byte, probeUID string) error {
	var node struct {
		Status struct{ Allocatable map[string]string }
	}
	if json.Unmarshal(nodeJSON, &node) != nil {
		return errors.New("invalid node capacity")
	}
	total := map[string]*big.Rat{}
	for resource, budget := range map[string]string{"cpu": "3500m", "memory": "14Gi", "ephemeral-storage": "48Gi"} {
		total[resource], _ = quantity(budget)
		if node.Status.Allocatable[resource] == "" {
			return errors.New("missing allocatable capacity")
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(string(reservations)))
	seenHeader, skip := false, false
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		switch fields[0] {
		case "P":
			if len(fields) != 3 || fields[1] == "" {
				return errors.New("invalid reservation identity")
			}
			seenHeader = true
			skip = fields[1] == probeUID || fields[2] == "Succeeded" || fields[2] == "Failed"
		case "R":
			if !seenHeader || len(fields) != 4 {
				return errors.New("incomplete reservations")
			}
			if skip {
				continue
			}
			for i, resource := range []string{"cpu", "memory", "ephemeral-storage"} {
				value, err := quantity(fields[i+1])
				if err != nil {
					return err
				}
				total[resource].Add(total[resource], value)
			}
		default:
			return errors.New("unknown reservation record")
		}
	}
	if scanner.Err() != nil || !seenHeader {
		return errors.New("missing complete reservation read")
	}
	for resource, reserved := range total {
		allocatable, err := quantity(node.Status.Allocatable[resource])
		if err != nil {
			return err
		}
		if reserved.Cmp(allocatable) > 0 {
			return errors.New("insufficient allocatable headroom")
		}
	}
	return nil
}

func verifyQuota(data []byte) error {
	var quotas struct {
		Items []struct {
			Spec struct {
				Scopes        []string
				ScopeSelector json.RawMessage
				Hard          map[string]string
			}
			Status struct{ Hard, Used map[string]string }
		}
	}
	if json.Unmarshal(data, &quotas) != nil || quotas.Items == nil {
		return errors.New("invalid quota read")
	}
	for _, q := range quotas.Items {
		if len(q.Spec.Scopes) != 0 || len(q.Spec.ScopeSelector) != 0 {
			return errors.New("unresolved scoped quota")
		}
		if len(q.Spec.Hard) == 0 {
			return errors.New("missing declared quota limits")
		}
		for resource, needed := range map[string]string{
			"pods": "1", "count/pods": "1", "requests.cpu": "3", "requests.memory": "12Gi", "requests.ephemeral-storage": "32Gi",
			"limits.cpu": "3500m", "limits.memory": "14Gi", "limits.ephemeral-storage": "48Gi",
			"cpu": "3", "memory": "12Gi", "ephemeral-storage": "32Gi",
		} {
			hard, exists := q.Spec.Hard[resource]
			if !exists {
				if _, stale := q.Status.Hard[resource]; stale {
					return errors.New("stale quota limit")
				}
				continue
			}
			accounted, exists := q.Status.Hard[resource]
			if !exists {
				return errors.New("missing quota limit accounting")
			}
			used, exists := q.Status.Used[resource]
			if !exists {
				return errors.New("incomplete quota accounting")
			}
			limit, err := quantity(hard)
			if err != nil {
				return err
			}
			observed, err := quantity(accounted)
			if err != nil || observed.Cmp(limit) != 0 {
				return errors.New("stale quota limit accounting")
			}
			current, err := quantity(used)
			if err != nil {
				return err
			}
			additional, _ := quantity(needed)
			if new(big.Rat).Add(current, additional).Cmp(limit) > 0 {
				return errors.New("insufficient namespace quota")
			}
		}
	}
	return nil
}
