package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var forwardAddressPattern = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:([0-9]{1,5}) -> 8080$`)

func forwardPort(line string) (string, bool) {
	match := forwardAddressPattern.FindStringSubmatch(line)
	if len(match) != 2 {
		return "", false
	}
	port, err := strconv.Atoi(match[1])
	return match[1], err == nil && port > 0 && port <= 65535
}
func sessionAccount(data []byte, expected string) (string, error) {
	var session object
	if json.Unmarshal(data, &session) != nil {
		return "", errors.New("session-json")
	}
	account := text(child(session, "primaryAccounts"), "urn:ietf:params:jmap:mail")
	accounts := child(session, "accounts")
	entry, ok := accounts[account].(map[string]any)
	if !mailboxIDPattern.MatchString(account) || (expected != "" && expected != account) || !ok || (entry["isReadOnly"] != nil && entry["isReadOnly"] != false) {
		return "", errors.New("session-account")
	}
	return account, nil
}
func (c *mailboxClient) session(ctx context.Context) error {
	data, err := c.request(ctx, http.MethodGet, "/jmap/session", "", nil)
	if err != nil {
		return err
	}
	account, err := sessionAccount(data, c.account)
	if err != nil {
		return err
	}
	c.account = account
	return nil
}

// kubectl's forwarding stream stays private. The only address it may supply is
// its own ephemeral IPv4 loopback port; neither the JMAP session nor a backup
// can select a destination for the account credential.
func (d *recoveryDriver) forward(ctx context.Context, ns, pod, uid string) (string, func(), error) {
	verify := func() error {
		if ns == "private-mail" && pod == "private-mail-0" && uid == d.source.podUID {
			o, found, err := d.lookup(ctx, "Pod", pod, ns)
			if err != nil || !found || text(child(o, "metadata"), "uid") != uid {
				return errors.New("forward-source")
			}
			return nil
		}
		ref := ownedReference{kind: "Pod", name: pod, namespace: ns, uid: uid}
		if ns != d.i.namespace || pod != "reader" {
			return errors.New("forward-scope")
		}
		o, err := d.owned(ctx, ref)
		if err != nil || !admittedRecoveryObject(recoveryReader(d.i, d.image), o) {
			return errors.New("forward-reader")
		}
		return nil
	}
	if err := verify(); err != nil {
		return "", nil, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, "kubectl", "--context=admin@prod", "--namespace="+ns, "port-forward", "pod/"+pod, "0:8080", "--address=127.0.0.1", "--pod-running-timeout=15s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, errors.New("forward-pipe")
	}
	cmd.Stderr = io.Discard
	if cmd.Start() != nil {
		cancel()
		return "", nil, errors.New("forward-start")
	}
	ready := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024), 4096)
		for scanner.Scan() {
			if port, ok := forwardPort(scanner.Text()); ok {
				select {
				case ready <- port:
				default:
				}
			}
		}
		_ = cmd.Wait()
		close(done)
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	select {
	case port := <-ready:
		if err := verify(); err != nil {
			stop()
			return "", nil, err
		}
		return "http://127.0.0.1:" + port, stop, nil
	case <-done:
		stop()
		return "", nil, errors.New("forward-exited")
	case <-ctx.Done():
		stop()
		return "", nil, errors.New("forward-cancelled")
	case <-time.After(15 * time.Second):
		stop()
		return "", nil, errors.New("forward-timeout")
	}
}
