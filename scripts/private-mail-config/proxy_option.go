package main

import (
	"net/netip"
	"strings"
)

// proxyOption leaves legacy private configuration intact unless a deployment
// explicitly supplies its non-secret transport choice.
type proxyOption struct{ value *bool }

// String implements flag.Value without private input values.
func (o *proxyOption) String() string {
	if o.value == nil {
		return ""
	}
	if *o.value {
		return "true"
	}
	return "false"
}

// Set accepts only an explicit, unambiguous transport choice.
func (o *proxyOption) Set(value string) error {
	if value != "true" && value != "false" {
		return errConfiguration
	}
	enabled := value == "true"
	o.value = &enabled
	return nil
}

// IsBoolFlag supports the standard bare enabling flag as well as =false.
func (*proxyOption) IsBoolFlag() bool { return true }

// mailProxySourceOption names the private IPv4 address of the mail server's
// own load balancer. Only the four public mail listeners trust PROXY headers,
// and only from this address. Without it they trust none.
type mailProxySourceOption struct{ value string }

// String implements flag.Value.
func (o *mailProxySourceOption) String() string { return o.value }

// Set accepts exactly one private IPv4 address in canonical form.
func (o *mailProxySourceOption) Set(value string) error {
	if !validMailProxySource(value) {
		return errConfiguration
	}
	o.value = value
	return nil
}

// validMailProxySource accepts one private IPv4 address written canonically:
// no prefix length, zone, IPv6 form, loopback or public address.
func validMailProxySource(value string) bool {
	address, err := netip.ParseAddr(value)
	return err == nil && address.Is4() && address.String() == value && address.IsPrivate()
}

// mailProxySourceTrust reports whether a listener's stored trust is a single
// private IPv4 address, written alone or as its /32. The plan readback
// separately requires the exact address the deployment configured.
func mailProxySourceTrust(trust map[string]bool) bool {
	if len(trust) != 1 {
		return false
	}
	for network, enabled := range trust {
		return enabled && validMailProxySource(strings.TrimSuffix(network, "/32"))
	}
	return false
}
