# Cloudflared

Cloudflared is a tunneling daemon that proxies traffic through the Cloudflare network. It is used to make the Kubernetes cluster available on the internet.

Tunnels are locally managed — ingress rules are defined declaratively in the Helm release values rather than in the Cloudflare dashboard.

- [Documentation](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
- [Helm Chart](https://github.com/cloudflare/helm-charts/tree/main/charts/cloudflare-tunnel)
