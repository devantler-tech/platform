package retirementproof_test

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
)

func instanceCases(phase string) []decisionCase {
	cases := []decisionCase{}
	for _, resource := range []string{"engines", "replicas", "enginefrontends", "volumeattachments"} {
		kind := map[string]string{"engines": "Engine", "replicas": "Replica", "enginefrontends": "EngineFrontend", "volumeattachments": "VolumeAttachment"}[resource]
		name := "scratch-" + resource
		if resource == "volumeattachments" {
			name = "scratch-backend"
		}
		child := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "longhorn.io/v1beta2", "kind": kind,
			"metadata": map[string]any{"name": name, "namespace": "longhorn-system", "uid": "uid-scratch-" + resource, "resourceVersion": "1", "finalizers": []any{"longhorn.io"},
				"ownerReferences": []any{map[string]any{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": "scratch-backend", "uid": "uid-scratch-backend"}},
				"labels":          map[string]any{"longhornvolume": "scratch-backend"},
				"annotations":     map[string]any{"storage.platform.devantler.tech/retirement-checkpoint": "5678-" + strings.Repeat("a", 32)}},
			"spec":   map[string]any{"volumeName": "scratch-backend", "dataEngine": phase, "desireState": "stopped", "nodeID": "prod-worker-1"},
			"status": map[string]any{"ownerID": "prod-worker-1", "instanceManagerName": "scratch-im", "currentState": "stopped", "starting": false, "started": false},
		}}
		if resource == "replicas" || resource == "enginefrontends" {
			child.Object["spec"].(map[string]any)["engineName"] = "scratch-engines"
		}
		if resource == "volumeattachments" {
			child.Object["spec"] = map[string]any{"volume": "scratch-backend", "attachmentTickets": map[string]any{}}
			child.Object["status"] = map[string]any{"attachmentTicketStatuses": map[string]any{}}
		}
		add := func(name string, mutate func(*unstructured.Unstructured), operation admission.Operation, selected, allowed bool) {
			current := child.DeepCopy()
			mutate(current)
			var old runtime.Object = child.DeepCopy()
			if operation == admission.Create {
				old = nil
			}
			cases = append(cases, decisionCase{name: resource + "/" + name, resource: resource, old: old, current: current, operation: operation, selected: selected, allowed: allowed})
		}
		add("stopped controller status", func(*unstructured.Unstructured) {}, admission.Update, true, true)
		cases[len(cases)-1].subresource = "status"
		add("native finalizer removal", func(p *unstructured.Unstructured) { p.SetFinalizers(nil) }, admission.Update, true, true)
		add("checkpoint metadata write", func(*unstructured.Unstructured) {}, admission.Update, true, true)
		cases[len(cases)-1].old.(*unstructured.Unstructured).SetAnnotations(nil)
		add("missing checkpoint", func(p *unstructured.Unstructured) { p.SetAnnotations(nil) }, admission.Update, true, false)
		add("wrong checkpoint", func(p *unstructured.Unstructured) {
			p.SetAnnotations(map[string]string{"storage.platform.devantler.tech/retirement-checkpoint": "foreign"})
		}, admission.Update, true, false)
		add("replacement UID", func(p *unstructured.Unstructured) { p.SetUID("replacement") }, admission.Update, true, false)
		add("foreign owner", func(p *unstructured.Unstructured) { p.SetOwnerReferences([]metav1.OwnerReference{{UID: "foreign"}}) }, admission.Update, true, false)
		add("missing owner", func(p *unstructured.Unstructured) { p.SetOwnerReferences(nil) }, admission.Update, true, false)
		add("missing native selector label", func(p *unstructured.Unstructured) { p.SetLabels(nil) }, admission.Update, true, false)
		add("spec mutation", func(p *unstructured.Unstructured) { p.Object["spec"].(map[string]any)["nodeID"] = "foreign" }, admission.Update, true, false)
		add("metadata namespace escape", func(p *unstructured.Unstructured) { p.SetNamespace("foreign") }, admission.Update, true, false)
		add("original name recreation", func(p *unstructured.Unstructured) { p.SetUID("") }, admission.Create, true, false)
		add("original name foreign association", func(p *unstructured.Unstructured) {
			p.SetUID("foreign")
			p.SetOwnerReferences(nil)
			key := "volumeName"
			if resource == "volumeattachments" {
				key = "volume"
			}
			p.Object["spec"].(map[string]any)[key] = "foreign"
		}, admission.Create, true, false)
		add("alias with original association", func(p *unstructured.Unstructured) { p.SetName("alias"); p.SetUID("") }, admission.Create, true, false)
		add("old original new unrelated", func(p *unstructured.Unstructured) {
			p.SetName("foreign")
			p.SetOwnerReferences(nil)
			key := "volumeName"
			if resource == "volumeattachments" {
				key = "volume"
			}
			p.Object["spec"].(map[string]any)[key] = "foreign"
		}, admission.Update, true, false)
		add("unrelated create", func(p *unstructured.Unstructured) {
			p.SetName("foreign")
			p.SetOwnerReferences(nil)
			p.SetLabels(nil)
			key := "volumeName"
			if resource == "volumeattachments" {
				key = "volume"
			}
			p.Object["spec"].(map[string]any)[key] = "foreign"
		}, admission.Create, false, false)
		add("native selector label without owner or spec reference", func(p *unstructured.Unstructured) {
			p.SetName("foreign")
			p.SetOwnerReferences(nil)
			key := "volumeName"
			if resource == "volumeattachments" {
				key = "volume"
			}
			p.Object["spec"].(map[string]any)[key] = "foreign"
		}, admission.Create, true, false)
		for _, operation := range []admission.Operation{admission.Create, admission.Update} {
			foreign := child.DeepCopy()
			foreign.SetName("scratch-engines")
			if resource == "engines" {
				foreign.SetName("scratch-replicas")
			}
			foreign.SetUID("foreign")
			foreign.SetOwnerReferences(nil)
			foreign.SetLabels(nil)
			key := "volumeName"
			if resource == "volumeattachments" {
				key = "volume"
			}
			foreign.Object["spec"].(map[string]any)[key] = "foreign"
			var old runtime.Object
			if operation == admission.Update {
				old = foreign.DeepCopy()
			}
			cases = append(cases, decisionCase{name: resource + "/other-kind name/" + string(operation), resource: resource, old: old, current: foreign, operation: operation, selected: false})
		}
		if resource == "volumeattachments" {
			add("unsatisfied request", func(p *unstructured.Unstructured) {
				p.Object["spec"].(map[string]any)["attachmentTickets"] = map[string]any{"pending": map[string]any{}}
			}, admission.Update, true, false)
			add("status request", func(p *unstructured.Unstructured) {
				p.Object["status"].(map[string]any)["attachmentTicketStatuses"] = map[string]any{"pending": map[string]any{"satisfied": false}}
			}, admission.Update, true, false)
		} else {
			add("native instance manager clearing", func(p *unstructured.Unstructured) {
				delete(p.Object["status"].(map[string]any), "instanceManagerName")
			}, admission.Update, true, true)
			for _, flag := range []string{"starting", "started"} {
				add(flag+" after checkpoint", func(p *unstructured.Unstructured) { p.Object["status"].(map[string]any)[flag] = true }, admission.Update, true, false)
				cases[len(cases)-1].subresource = "status"
			}
			add("owner process changed", func(p *unstructured.Unstructured) { p.Object["status"].(map[string]any)["ownerID"] = "foreign" }, admission.Update, true, false)
			add("manager process changed", func(p *unstructured.Unstructured) {
				p.Object["status"].(map[string]any)["instanceManagerName"] = "foreign"
			}, admission.Update, true, false)
			add("running process", func(p *unstructured.Unstructured) { p.Object["status"].(map[string]any)["currentState"] = "running" }, admission.Update, true, false)
			add("unknown process state", func(p *unstructured.Unstructured) { delete(p.Object["status"].(map[string]any), "currentState") }, admission.Update, true, false)
		}
	}
	return cases
}
