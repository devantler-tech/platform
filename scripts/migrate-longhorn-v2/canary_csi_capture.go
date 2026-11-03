package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os/exec"
	"sync"
)

// A completed DataUpload can outlive its temporary PVC, PV and backend. Start
// gap-free API watches before creating the backup, and retain their observed
// identities. This evidence never authorizes deletion of an observed name.
type csiCapture struct {
	mu      sync.Mutex
	run     string
	objects map[string]map[string]object
	err     error
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

var csiCollections = map[string]string{
	"persistentvolumeclaims": "/api/v1/namespaces/velero/persistentvolumeclaims",
	"persistentvolumes":      "/api/v1/persistentvolumes",
	"volumes.longhorn.io":    "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes",
}

// csiInventory anchors the following fixed-scope watch to a complete native
// collection snapshot rather than kubectl's synthetic List printer.
func csiInventory(ctx context.Context, c client, query string) ([]object, string, error) {
	path, ok := csiCollections[query]
	if !ok {
		return nil, "", errors.New("CSI inventory is outside its fixed scope")
	}
	// kubectl's ordinary JSON printer synthesizes kind: List with an empty
	// resourceVersion. Read the native collection so its snapshot can anchor
	// the following watch without missing transient provisioning resources.
	data, err := c.kubectl(ctx, nil, "get", "--raw="+path+"?limit=500")
	if err != nil {
		return nil, "", err
	}
	var list object
	if json.Unmarshal(data, &list) != nil || text(list, "metadata", "resourceVersion") == "" || len(text(list, "metadata", "resourceVersion")) > 256 {
		return nil, "", errors.New("CSI watch cannot start without a complete native list resource version")
	}
	items, err := decodeList(data)
	return items, text(list, "metadata", "resourceVersion"), err
}

// newCSICapture creates a run-scoped inventory for the three storage collections.
func newCSICapture(run string) *csiCapture {
	return &csiCapture{run: run, objects: map[string]map[string]object{
		"persistentvolumeclaims": {}, "persistentvolumes": {}, "volumes.longhorn.io": {},
	}}
}

// observe retains bounded original identities, including short-lived bound
// scratch objects that native transfer cleanup may remove before the next poll.
func (p *csiCapture) observe(query string, o object) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	uid := text(o, "metadata", "uid")
	if uid == "" || text(o, "metadata", "resourceVersion") == "" {
		return errors.New("CSI watch object has no complete identity")
	}
	class := text(o, "spec", "storageClassName")
	if query != "volumes.longhorn.io" {
		if class != "lh-canary-v1-"+p.run && class != "lh-canary-v2-"+p.run {
			return nil
		}
		if query == "persistentvolumeclaims" && (text(o, "metadata", "namespace") != "velero" || text(o, "spec", "volumeName") == "") {
			return nil
		}
		if query == "persistentvolumes" && text(o, "spec", "claimRef", "namespace") != "velero" {
			return nil
		}
	}
	items, ok := p.objects[query]
	if !ok {
		return errors.New("CSI capture request is outside its fixed scope")
	}
	if len(items) >= 512 {
		return errors.New("CSI capture exceeded its bounded identity inventory")
	}
	// Keep the original bound graph, including a clone's original dataSource,
	// before native cleanup or download rebinding changes those fields.
	if _, exists := items[uid]; !exists {
		items[uid] = o
	}
	return nil
}

// proof joins exactly one owned CSI scratch graph and rejects PV name reuse;
// these observations are evidence and never deletion authority.
func (p *csiCapture) proof(operation, source object, class string) (csiScratchEvidence, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return csiScratchEvidence{}, p.err
	}
	var proofs []csiScratchEvidence
	for _, claim := range p.objects["persistentvolumeclaims"] {
		for _, pv := range p.objects["persistentvolumes"] {
			if text(pv, "metadata", "name") != text(claim, "spec", "volumeName") {
				continue
			}
			for _, volume := range p.objects["volumes.longhorn.io"] {
				if text(volume, "metadata", "name") != text(pv, "spec", "csi", "volumeHandle") {
					continue
				}
				if proof, err := verifyCSIScratchBinding(claim, pv, volume, operation, source, class); err == nil {
					proofs = append(proofs, proof)
				}
			}
		}
	}
	if len(proofs) != 1 {
		return csiScratchEvidence{}, errors.New("the CSI operation's independent storage graph was not observed exactly once")
	}
	proof := proofs[0]
	for _, pv := range p.objects["persistentvolumes"] {
		if text(pv, "metadata", "name") == proof.PV.Name && text(pv, "metadata", "uid") != proof.PV.UID {
			return proof, errors.New("the CSI temporary PV name was reused")
		}
	}
	return proof, nil
}

// readCSIWatch rejects stream gaps, incomplete objects and unsupported events
// instead of accepting a partial identity history.
func readCSIWatch(input io.Reader, observe func(object) error) error {
	decoder := json.NewDecoder(input)
	for {
		var event struct {
			Type   string `json:"type"`
			Object object `json:"object"`
		}
		if decoder.Decode(&event) != nil {
			return errors.New("CSI watch ended before the recovery proof completed")
		}
		if event.Type == "BOOKMARK" {
			continue
		}
		if event.Type != "ADDED" && event.Type != "MODIFIED" && event.Type != "DELETED" {
			return errors.New("CSI watch reported an error or unsupported event")
		}
		if text(event.Object, "metadata", "uid") == "" || text(event.Object, "metadata", "resourceVersion") == "" {
			return errors.New("CSI watch event identity is incomplete")
		}
		if err := observe(event.Object); err != nil {
			return err
		}
	}
}

// start lists then streams each fixed collection from its resource version;
// the parent deadline bounds native watches and any failed stream poisons proof.
func (p *csiCapture) start(ctx context.Context, c client) error {
	watchCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	for _, query := range []string{"persistentvolumeclaims", "persistentvolumes", "volumes.longhorn.io"} {
		items, rv, err := csiInventory(watchCtx, c, query)
		if err != nil {
			p.stop()
			return err
		}
		for _, item := range items {
			if err = p.observe(query, item); err != nil {
				p.stop()
				return err
			}
		}
		args := []string{"--context=" + c.context, "--request-timeout=0", "get", "--raw=" + csiCollections[query] + "?watch=true&allowWatchBookmarks=true&resourceVersion=" + url.QueryEscape(rv)}
		cmd := exec.CommandContext(watchCtx, "kubectl", args...)
		cmd.Stderr = io.Discard
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			p.stop()
			return err
		}
		if err = cmd.Start(); err != nil {
			p.stop()
			return errors.New("CSI identity watch could not start")
		}
		p.wg.Go(func() {
			err := readCSIWatch(stdout, func(o object) error { return p.observe(query, o) })
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if watchCtx.Err() == nil {
				p.mu.Lock()
				if p.err == nil {
					p.err = err
				}
				p.mu.Unlock()
			}
		})
	}
	return nil
}

// stop cancels and joins only the native watch processes owned by this capture.
func (p *csiCapture) stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// health returns any observed stream failure after synchronized shutdown.
func (p *csiCapture) health() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
