package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type boundaryAPI struct {
	objects           map[string]object
	writes            int
	lostAck, replaced bool
}

func (a *boundaryAPI) call(_ context.Context, input []byte, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("empty-command")
	}
	switch args[0] {
	case "create":
		a.writes++
		var o object
		if json.Unmarshal(input, &o) != nil {
			return nil, errors.New("manifest")
		}
		child(o, "metadata")["uid"] = "created-resource-uid"
		a.objects[text(o, "kind")] = o
		if a.lostAck {
			return nil, errors.New("connection-lost")
		}
		return json.Marshal(o)
	case "get":
		if len(args) < 3 {
			return nil, errors.New("get")
		}
		o, ok := a.objects[args[1]]
		if !ok {
			return nil, nil
		}
		data, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		if a.replaced {
			var changed object
			_ = json.Unmarshal(data, &changed)
			child(changed, "metadata")["uid"] = "foreign-resource-uid"
			return json.Marshal(changed)
		}
		return data, nil
	case "delete":
		a.writes++
		var options object
		if json.Unmarshal(input, &options) != nil || text(child(options, "preconditions"), "uid") != "created-resource-uid" || len(args) != 3 || !strings.HasPrefix(args[1], "--raw=/api/v1/namespaces/email-data-recovery-") {
			return nil, errors.New("unsafe-delete")
		}
		delete(a.objects, "Namespace")
		return []byte(`{}`), nil
	}
	return nil, errors.New("unexpected-command")
}
func newBoundaryDriver() (*recoveryDriver, *boundaryAPI) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	a := &boundaryAPI{objects: map[string]object{}}
	return &recoveryDriver{i: i, k: a, refs: map[string]ownedReference{}}, a
}

func TestServingAndForeignResourcesAreRefusedBeforeKubernetesWrite(t *testing.T) {
	d, a := newBoundaryDriver()
	for _, o := range []object{{"kind": "PersistentVolumeClaim", "metadata": object{"name": "private-mail-data", "namespace": "private-mail"}}, {"kind": "Secret", "metadata": object{"name": "storage-key", "namespace": "another"}}} {
		if _, err := d.create(context.Background(), o); err == nil {
			t.Fatal("foreign mutation was allowed")
		}
	}
	if a.writes != 0 {
		t.Fatal("foreign resource reached the Kubernetes write boundary")
	}
}
func TestOwnedResourceDeletionUsesCreatedUID(t *testing.T) {
	d, a := newBoundaryDriver()
	o := recoveryResources(d.i, "test-key", "test-admin")[0]
	ref, err := d.create(context.Background(), o)
	if err != nil {
		t.Fatal("owned create failed")
	}
	if err = d.remove(context.Background(), ref); err != nil {
		t.Fatal("owned cleanup failed")
	}
	if a.writes != 2 {
		t.Fatal("owned resource was not created and deleted")
	}
}
func TestReplacementAndLostAcknowledgementRetainResources(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		d, a := newBoundaryDriver()
		ref, err := d.create(context.Background(), recoveryResources(d.i, "key", "admin")[0])
		if err != nil {
			t.Fatal(err)
		}
		a.replaced = true
		if d.remove(context.Background(), ref) == nil || a.writes != 1 {
			t.Fatal("a replacement resource was deleted")
		}
	})
	t.Run("lost-ack", func(t *testing.T) {
		d, a := newBoundaryDriver()
		a.lostAck = true
		if _, err := d.create(context.Background(), recoveryResources(d.i, "key", "admin")[0]); err == nil || !d.uncertain {
			t.Fatal("lost create acknowledgement was treated as a known failure")
		}
		if d.remove(context.Background(), ownedReference{kind: "Namespace", name: d.i.namespace, uid: "created-resource-uid"}) == nil || a.writes != 1 {
			t.Fatal("uncertain ownership allowed destructive cleanup")
		}
	})
}

func TestFreshRecoveryBackupIsNeverManuallyDeleted(t *testing.T) {
	d, a := newBoundaryDriver()
	ref, err := d.create(context.Background(), backupManifest(d.i))
	if err != nil {
		t.Fatal("owned backup could not be created")
	}
	if d.remove(context.Background(), ref) == nil || a.writes != 1 {
		t.Fatal("recovery bypassed native backup retention")
	}
}
