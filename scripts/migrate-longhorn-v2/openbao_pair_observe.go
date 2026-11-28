package main

import "context"

type openBaoPairObservation struct {
	stateful, service, budget object
	pods, nodes, routes       []object
}

func readOpenBaoPairObservation(ctx context.Context, c client) (openBaoPairObservation, error) {
	var current openBaoPairObservation
	var err error
	if current.stateful, err = c.get(ctx, "statefulset", openBaoNamespace, openBaoNamespace, false); err != nil {
		return current, err
	}
	if current.service, err = c.get(ctx, "service", openBaoNamespace, "openbao-active", false); err != nil {
		return current, err
	}
	if current.budget, err = c.get(ctx, "poddisruptionbudget", openBaoNamespace, openBaoNamespace, false); err != nil {
		return current, err
	}
	for _, query := range []struct {
		kind, namespace string
		result          *[]object
	}{
		{"pods", openBaoNamespace, &current.pods},
		{"nodes", "", &current.nodes},
		{"endpointslices.discovery.k8s.io", openBaoNamespace, &current.routes},
	} {
		*query.result, err = c.list(ctx, query.kind, query.namespace)
		if err != nil {
			return current, err
		}
	}
	routes := current.routes[:0]
	for _, route := range current.routes {
		if text(route, "metadata", "labels", "kubernetes.io/service-name") == "openbao-active" {
			routes = append(routes, route)
		}
	}
	current.routes = routes
	return current, nil
}

func observeOpenBaoPairPlan(ctx context.Context, c client, member, run, source string) (openBaoPairPlan, error) {
	if !knownOpenBaoMember(member) || !validBenchmarkOwner(run, source) {
		return openBaoPairPlan{}, errOpenBaoProviderStorage
	}
	current, err := readOpenBaoPairObservation(ctx, c)
	if err != nil {
		return openBaoPairPlan{}, err
	}
	claims, err := c.list(ctx, "persistentvolumeclaims", openBaoNamespace)
	if err != nil {
		return openBaoPairPlan{}, err
	}
	pvs, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return openBaoPairPlan{}, err
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return openBaoPairPlan{}, err
	}
	original, err := bindOpenBaoProviderStorage(member, current.stateful, current.pods, claims, pvs, attachments)
	if err != nil {
		return openBaoPairPlan{}, err
	}
	node := uniqueOpenBaoObject(current.nodes, original.Node)
	if !recoveredNativeObject(node, "v1", "Node", "") || !ready(valueObject(node, "status")) ||
		value(node, "spec", "unschedulable") == true ||
		text(node, "metadata", "labels", "kubernetes.io/hostname") != original.Node {
		return openBaoPairPlan{}, errOpenBaoProviderStorage
	}
	survivors, err := bindOpenBaoSurvivors(original, current.stateful, current.pods, current.nodes)
	if err != nil || !openBaoActiveRoute(current.service, current.routes, current.pods, survivors) ||
		!openBaoDisruptionBudget(current, 3) {
		return openBaoPairPlan{}, errOpenBaoProviderStorage
	}
	digest, err := openBaoProviderStorageDigest(original)
	if err != nil {
		return openBaoPairPlan{}, err
	}
	plan := openBaoPairPlan{Run: run, Source: source, Input: openBaoPairInput{Member: member, Digest: digest},
		Original: original, Survivors: survivors}
	if err := verifyOpenBaoPairPlan(plan, run, source); err != nil {
		return openBaoPairPlan{}, err
	}
	after, err := readOpenBaoPairObservation(ctx, c)
	if err != nil || verifyOpenBaoSurvivors(original, after.stateful, survivors, after.pods, after.nodes, after.service, after.routes) != nil ||
		benchmarkIdentity(uniqueOpenBaoObject(after.pods, member)) != original.Member ||
		benchmarkIdentity(uniqueOpenBaoObject(after.nodes, original.Node)) != benchmarkIdentity(node) ||
		benchmarkIdentity(after.budget) != benchmarkIdentity(current.budget) || !openBaoDisruptionBudget(after, 3) {
		return openBaoPairPlan{}, errOpenBaoProviderStorage
	}
	return plan, nil
}
