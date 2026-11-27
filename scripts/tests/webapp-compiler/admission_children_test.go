package compiler_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	kroapi "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	gecompiler "github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyWebAppAdmissionExport(ctx context.Context, t *testing.T, api client.Client, engine *gecompiler.Compiler, definition *kroapi.ResourceGraphDefinition, app *unstructured.Unstructured) {
	t.Helper()
	directory := t.TempDir()
	source, output := filepath.Join(directory, "webapp.json"), filepath.Join(directory, "children.json")
	data, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	renderWebAppAdmissionChildren(ctx, t, api, engine, definition, source, output)
	data, err = os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var objects []unstructured.Unstructured
	if err := json.Unmarshal(data, &objects); err != nil {
		t.Fatal(err)
	}
	var nodes []string
	for _, child := range objects {
		labels := child.GetLabels()
		if labels[metadata.InstanceKindLabel] != "WebApp" || labels[metadata.InstanceIDLabel] == "" ||
			labels[metadata.InstanceLabel] != app.GetName() || labels[applyset.ApplysetPartOfLabel] != applyset.ID(app) {
			t.Fatal("exported child lost its WebApp ownership")
		}
		nodes = append(nodes, labels[metadata.NodeIDLabel])
	}
	if !slices.Equal(nodes, []string{"deployment", "service", "httpRoute"}) {
		t.Fatalf("incomplete generated workload inventory: %v", nodes)
	}
}

func renderWebAppAdmissionChildren(ctx context.Context, t *testing.T, api client.Client, engine *gecompiler.Compiler, definition *kroapi.ResourceGraphDefinition, source, output string) {
	t.Helper()
	const maxInput = 1 << 20
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(input, maxInput+1))
	if err := errors.Join(readErr, input.Close()); err != nil {
		t.Fatal(err)
	}
	var app unstructured.Unstructured
	if len(raw) > maxInput || json.Unmarshal(raw, &app) != nil ||
		app.GetAPIVersion() != "kro.run/v1alpha1" || app.GetKind() != "WebApp" ||
		app.GetNamespace() == "" || app.GetName() != app.GetNamespace() {
		t.Fatal("expected one namespace-bound WebApp JSON object")
	}
	namespace := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": app.GetNamespace()},
	}}
	if err := api.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(ctx, &app, &client.CreateOptions{FieldValidation: "Strict"}); err != nil {
		t.Fatal(err)
	}

	children, ignored := resolveChildren(t, engine, definition, &app)
	expected := []struct{ node, kind, version string }{
		{"deployment", "Deployment", "apps/v1"},
		{"service", "Service", "v1"},
		{"httpRoute", "HTTPRoute", "gateway.networking.k8s.io/v1"},
	}
	if len(children) != len(expected) || len(ignored) != 0 {
		t.Fatal("expected the Deployment, Service and HTTPRoute")
	}
	instanceLabels := metadata.NewInstanceLabeler(&app, true)
	managedLabels := metadata.NewNodeLabeler()
	kroLabels := metadata.NewKROMetaLabeler()
	applysetID := applyset.ID(&app)
	var objects []*unstructured.Unstructured
	for _, want := range expected {
		child := children[want.node]
		if child == nil || child.GetKind() != want.kind || child.GetAPIVersion() != want.version ||
			child.GetName() != app.GetName() || child.GetNamespace() != app.GetNamespace() {
			t.Fatalf("unexpected identity for WebApp child %s", want.node)
		}
		// Use KRO's label helpers, including the real API-assigned instance UID.
		kroLabels.ApplyLabels(child)
		instanceLabels.ApplyLabels(child)
		managedLabels.ApplyLabels(child)
		labels := child.GetLabels()
		labels[metadata.NodeIDLabel] = metadata.NodeIDToken(want.node)
		labels[applyset.ApplysetPartOfLabel] = applysetID
		child.SetLabels(labels)
		objects = append(objects, child)
	}
	data, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}
