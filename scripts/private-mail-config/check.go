package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Exit statuses of the drift check. Both failures fail the scheduled Job.
const (
	checkMatches = 0
	checkDrift   = 1
	checkUnknown = 2
)

// checkCommand reads the serving configuration back and compares it with the
// declared plan. It never writes: a drift returns to the declared value at the
// next start, and this check reports it in between.
func checkCommand(args []string, log io.Writer) int {
	f := flag.NewFlagSet("check", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("input-dir", "/etc/private-mail", "mounted private inputs")
	endpoint := f.String("url", "", "serving management endpoint, http://host:port")
	var proxy proxyOption
	f.Var(&proxy, "proxy-client-ip", "explicit deployment transport override")
	var mailProxy mailProxySourceOption
	f.Var(&mailProxy, "mail-proxy-source", "private IPv4 address of the mail load balancer")
	unknown := func(reason string) int {
		_, _ = fmt.Fprintln(log, "Email configuration check could not run:", reason)
		return checkUnknown
	}
	if f.Parse(args) != nil || f.NArg() != 0 || !filepath.IsAbs(*dir) || !validCheckEndpoint(*endpoint) {
		return unknown("invalid arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 2*time.Minute)
	defer timeout()
	v, err := loadInputs(*dir)
	if err != nil {
		return unknown("private inputs")
	}
	if proxy.value != nil {
		v.UseProxyClientIP = *proxy.value
	}
	v.mailProxySource = mailProxy.value
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	m := management{
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		url:    *endpoint, user: v.Administrator + "@" + v.Domain, password: v.administratorPassword,
	}
	request, err := inventoryRequestFor(v.RuntimeVersion)
	if err != nil {
		return unknown("runtime version")
	}
	inventory, err := m.request(ctx, "/jmap", request)
	if err != nil {
		return unknown("inventory read")
	}
	// An unexpected listener, application or recipient is itself drift.
	if validateInventoryFor(inventory, v.RuntimeVersion) != nil || validateRecipientOwnership(inventory, v) != nil {
		_, _ = fmt.Fprintln(log, "Email configuration drift: the serving inventory differs from the declared one.")
		return checkDrift
	}
	if v.accountIDs, err = managedAccountIDs(inventory, v); err != nil {
		_, _ = fmt.Fprintln(log, "Email configuration drift: the managed accounts differ from the declared ones.")
		return checkDrift
	}
	plan, err := render(v)
	if err != nil {
		return unknown("plan")
	}
	if err := m.verify(ctx, plan); err != nil {
		if ctx.Err() != nil {
			return unknown("timed out")
		}
		// Readback does not separate a mismatch from an unreadable setting. The
		// object name is not private; its values are never printed.
		object := "a setting"
		var named objectError
		if errors.As(err, &named) && objectNamePattern.MatchString(named.object) {
			object = named.object
		}
		_, _ = fmt.Fprintf(log, "Email configuration drift in %s: it differs from the declared configuration or could not be read; the next start restores it.\n", object)
		return checkDrift
	}
	if _, err := fmt.Fprintln(log, "Email configuration matches the declared configuration."); err != nil {
		return checkUnknown
	}
	return checkMatches
}

// objectError names the registry object whose readback failed.
type objectError struct {
	object string
	err    error
}

func (e objectError) Error() string { return e.object + ": " + e.err.Error() }
func (e objectError) Unwrap() error { return e.err }

var objectNamePattern = regexp.MustCompile(`^[A-Z][A-Za-z]{1,63}$`)

// validCheckEndpoint accepts only a plain http://host:port origin. The check
// runs inside the cluster against the management listener.
func validCheckEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Host == "" || u.Port() == "" || u.Hostname() == "" {
		return false
	}
	return u.Path == "" && !u.ForceQuery && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}
