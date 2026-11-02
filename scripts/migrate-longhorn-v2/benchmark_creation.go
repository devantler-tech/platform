package main

import (
	"context"
	"errors"
	"reflect"
)

// Persist intent before CREATE. A missing API response or failed UID receipt
// leaves PendingCreate in the durable journal; recovery must never adopt that
// object from its name or labels. Only a successfully recorded UID is ownership.
func createBenchmarkScratch(ctx context.Context, c client, record *benchmarkRecord, desired object) (object, error) {
	kind := text(desired, "kind")
	assets, err := benchmarkResources(record.Phase, record.Run, record.Namespace)
	if err != nil || record.PendingCreate != "" || record.Recovery != nil || record.Cleaned || record.Resources[kind].UID != "" {
		return nil, errors.New("benchmark scratch creation is not a new owned step")
	}
	matched := false
	for _, expected := range assets {
		if reflect.DeepEqual(expected, desired) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, errors.New("scratch creation differs from the reviewed resource")
	}
	record.PendingCreate = kind
	if err := saveBenchmarkProgress(ctx, c, record.Source, record.Run, *record); err != nil {
		return nil, err
	}
	actual, err := c.create(ctx, record.Source, desired)
	if err != nil {
		return nil, err
	}
	if text(actual, "apiVersion") != text(desired, "apiVersion") || text(actual, "metadata", "namespace") != text(desired, "metadata", "namespace") {
		return nil, errors.New("created scratch scope differs; ownership remains unresolved")
	}
	if record.Resources == nil {
		record.Resources = map[string]benchmarkResource{}
	}
	record.Resources[kind] = benchmarkIdentity(actual)
	if kind == "Namespace" {
		record.NamespaceUID = text(actual, "metadata", "uid")
	}
	record.PendingCreate = ""
	if err := saveBenchmarkProgress(ctx, c, record.Source, record.Run, *record); err != nil {
		return nil, err
	}
	return actual, nil
}
