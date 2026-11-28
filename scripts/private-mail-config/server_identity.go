package main

import (
	"flag"
	"regexp"
	"strings"
)

// hostnameOption names one public host of the mail server. The deployment
// passes it as a reviewed, non-secret flag: the private settings are encrypted
// and also hold the account names, so a public name changes without them.
type hostnameOption struct{ value string }

// String implements flag.Value.
func (o *hostnameOption) String() string { return o.value }

// Set accepts one lowercase DNS name. render also requires it under the mail
// domain, which only the private settings name.
func (o *hostnameOption) Set(value string) error {
	if !validHostname(value) {
		return errConfiguration
	}
	o.value = value
	return nil
}

// validHostname accepts a lowercase DNS name of at least two labels.
func validHostname(value string) bool {
	return len(value) <= 253 && domainPattern.MatchString(value)
}

// hostnameUnder reports whether a valid name is a host under the mail domain.
func hostnameUnder(name, domain string) bool {
	return validHostname(name) && strings.HasSuffix(name, "."+domain)
}

// deploymentNames are the server's public names as the deployment gives them.
type deploymentNames struct{ hostname, webHostname hostnameOption }

// register declares the two name flags on a command's flag set.
func (n *deploymentNames) register(f *flag.FlagSet) {
	f.Var(&n.hostname, "hostname", "public mail server name; overrides the settings hostname")
	f.Var(&n.webHostname, "web-hostname", "public web host of the mail server")
}

// apply sets the deployment's names on loaded inputs. --hostname takes
// precedence over the settings' hostname: when it is given, it becomes the
// server's default hostname (SMTP greeting and EHLO name, Message-ID domain,
// and the host autoconfig gives IMAP and SMTP) and the settings' value is not
// used. When it is absent, the settings' hostname is used unchanged, so
// removing the flag is the rollback. render validates both names.
//
// --hostname also serves implicit-TLS submission on 465: the public name and
// the port its autoconfig offers ship together. Without the flag the setup
// writes nothing the previous setup image refuses, so a failed deployment can
// be healed back to that image (see implicitSubmissionRemoval).
func (n deploymentNames) apply(v *inputs) {
	if n.hostname.value != "" {
		v.Hostname = n.hostname.value
		v.implicitSubmission = true
	}
	v.webHostname = n.webHostname.value
}

// implicitSubmissionListener is the listener that serves submission with
// implicit TLS on 465. The previous setup image (0.16.27, the digest pinned
// before #6662) refuses any listener it does not know, this one included, and
// then keeps the server stopped.
const implicitSubmissionListener = "submissions"

// implicitSubmissionRemoval removes the implicit-TLS submission listener when
// the deployment does not ask for it. Leaving a stored one in place would keep
// a listener the plan no longer declares, so its readback could not be exact,
// and the previous setup image could never start on that store again. The
// removal is idempotent: the native CLI destroys only what matches the filter,
// and name is the listener's unique index, so it matches this one listener or
// none. An unsupported filter fails the apply rather than widening; the
// readback then requires that no listener of that name remains.
func implicitSubmissionRemoval() map[string]any {
	return map[string]any{"@type": "destroy", "object": "NetworkListener", "value": map[string]any{"name": implicitSubmissionListener}}
}

// advertisedServices declares what the server's autoconfig, autodiscover and
// PACC documents advertise (upstream common/src/config/network.rs and
// common/src/network/autoconfig). The upstream default also lists POP3 and
// ManageSieve, which this server does not serve. IMAP and SMTP follow the
// default hostname and offer implicit TLS only, so the documents give IMAP on
// 993 and submission on 465 (RFC 8314); submission on 587 stays served for
// clients set up by hand. JMAP and the DAV services name the web host when the
// deployment gives one, because the mail hostname serves no HTTPS.
func advertisedServices(webHostname string) map[string]any {
	var web any
	if webHostname != "" {
		web = webHostname
	}
	services := map[string]any{}
	for name, host := range map[string]any{"imap": nil, "smtp": nil, "jmap": web, "caldav": web, "carddav": web, "webdav": web} {
		services[name] = map[string]any{"hostname": host, "cleartext": false}
	}
	return services
}

// submissionListeners are the authenticated submission listeners: STARTTLS on
// 587 and implicit TLS on 465 (RFC 8314). Every rule that requires sign-in or
// limits the sender is built from this one list. A submission listener missing
// from any of those rules would accept mail without sign-in or sender checks.
// The rules name both whether or not the deployment serves 465, so a stored
// implicit-TLS listener is never left outside them.
var submissionListeners = []string{"submission", implicitSubmissionListener}

// onSubmission is an expression that is true on every submission listener.
func onSubmission() string {
	terms := make([]string, 0, len(submissionListeners))
	for _, name := range submissionListeners {
		terms = append(terms, "listener == '"+name+"'")
	}
	return "(" + strings.Join(terms, " || ") + ")"
}

// offSubmission is an expression that is true on every other listener.
func offSubmission() string {
	terms := make([]string, 0, len(submissionListeners))
	for _, name := range submissionListeners {
		terms = append(terms, "listener != '"+name+"'")
	}
	return "(" + strings.Join(terms, " && ") + ")"
}

// isSubmissionListener reports whether a listener name is a submission listener.
func isSubmissionListener(name string) bool {
	for _, listener := range submissionListeners {
		if name == listener {
			return true
		}
	}
	return false
}

// jmapIDPattern is a native JMAP ID: a number in the server's base32 alphabet,
// "abcdefghijklmnopqrstuvwxyz792013" (upstream utils/src/codec/base32_custom.rs).
var jmapIDPattern = regexp.MustCompile(`^[a-z012379]{1,13}$`)

// uploadRule refuses the HTTP upload endpoint except at the maintainer's own
// upload URL. The endpoint does not honor the JMAP permission list. The HTTP
// rule runs before sign-in, so it sees only the request: upstream gives it the
// listener, addresses, URL, path, method and headers, never the signed-in
// account (http/src/request.rs, http-proto/src/context.rs). The narrowest rule
// is therefore the account in the path, /jmap/upload/<account>/, the upload URL
// the JMAP session gives. Every other upload path, including the agent's and
// the administrator's own, stays refused.
//
// Upstream's upload does not check that the signed-in account owns the path's
// account (jmap/src/blob/upload.rs). Another signed-in account can therefore
// store a temporary upload in the maintainer's upload space, counted against
// its upload quota, but cannot read or use it. Before the maintainer's account
// exists, as on the first start of an empty store, or when its ID is not a
// native JMAP ID, uploads stay refused for every account.
func uploadRule(maintainerID string) map[string]any {
	refuse := "starts_with(path, '/jmap/upload/')"
	if jmapIDPattern.MatchString(maintainerID) {
		refuse += " && path != '/jmap/upload/" + maintainerID + "/'"
	}
	return map[string]any{"match": map[string]any{"0": map[string]any{"if": refuse, "then": "403"}}, "else": "200"}
}
