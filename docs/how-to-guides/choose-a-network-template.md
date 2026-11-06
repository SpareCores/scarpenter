# Choose a node network template

`spec.network.template` of an SCNodeClass decides how a node reaches the control
plane and how the control plane reaches the node's kubelet (for `kubectl logs`,
`exec` and metrics). Pick one of three. What each renders is in the
[node bootstrap reference](../reference/node-bootstrap.md#network-templates).

| | `tailscale` | `direct` | `custom` |
|---|---|---|---|
| Nodes need | outbound connectivity only | a route to the API server, and the control plane a route to the nodes | whatever your template sets up |
| Node address (`--node-ip`) | the node's tailscale IPv4 | the primary address | your template's |
| Fits | control planes outside the vendor (home lab, another cloud), nodes in several vendors | control plane in the same VPC or network; vendors that put public IPs on the interface | WireGuard, a corporate VPN, anything else |
| Needs besides | an auth key Secret; a tailnet or Headscale the API server is on | an API endpoint reachable from the nodes | a Butane template in a ConfigMap |

## With `tailscale`

1. Create an auth key in your tailnet (or Headscale): **ephemeral**,
   **pre-authorized**, **reusable**, **tagged** (for example `tag:k8s-node`).
   Limit the tag in the ACL to what nodes need: the API server and other nodes.
2. Put it in a Secret:

   ```bash
   kubectl -n scarpenter-system create secret generic ts-node-key \
     --from-literal=authkey=tskey-auth-EXAMPLE
   ```

3. Reference it:

   ```yaml
   spec:
     network:
       template: tailscale
       tailscale:
         version: v1.98.10                # a sysext-bakery tailscale release
         loginServer: ""                  # "" is Tailscale SaaS; a URL for Headscale
         authKeySecretRef: {name: ts-node-key, namespace: scarpenter-system, key: authkey}
         tags: [tag:k8s-node]
   ```

4. `--api-server-endpoint` must be an address the nodes reach **over the tailnet**
   (or publicly). The controller's `ServiceAccount` needs `get` on that Secret
   (the default RBAC grants it for the controller's own namespace only; for a Secret
   in another namespace add a Role and RoleBinding there, see `config/rbac/controller.yaml`).

Rotating the Secret (a new resourceVersion) drifts the nodes of the NodeClass
(they are replaced); the key's value is not part of any hash.

## With `direct`

```yaml
spec:
  network: {template: direct}
```

Nothing is installed. The kubelet registers with the node's primary address, so
the API server must reach it: on AWS that is the private address, which works
inside one VPC or over a peering, not from outside.

## With `custom`

Write a Butane config (`variant: flatcar`, version `1.1.0`) that sets up your
network. If the kubelet should advertise a specific address, have a unit that
runs before the kubelet write `SCARPENTER_NODE_IP_ARGS=--node-ip=<ip>` to
`/run/scarpenter/node-ip.env`, which the kubelet unit reads, and make the kubelet
wait for it (the `tailscale` template does exactly this, see
`pkg/bootstrap/templates/tailscale.bu`):

```yaml
variant: flatcar
version: 1.1.0
systemd:
  units:
    - name: example-vpn.service
      enabled: true
      contents: |
        [Unit]
        Description=Join the example VPN as {{ .NodeName }} ({{ .Vendor }}, {{ .Region }})
        Before=kubelet.service
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/usr/local/bin/join-example-vpn --name {{ .NodeName }}
        [Install]
        WantedBy=multi-user.target
    - name: kubelet.service
      dropins:
        - name: 30-example-vpn.conf
          contents: |
            [Unit]
            Requires=example-vpn.service
            After=example-vpn.service
```

Store it in a ConfigMap and reference it:

```yaml
spec:
  network:
    template: custom
    custom: {configMapRef: {name: net, namespace: scarpenter-system, key: net.bu}}
```

The template gets the [variables](../reference/node-bootstrap.md#template-variables)
(`.NodeName`, `.Region`, `.Labels`, ...). It replaces the network part only: the
kubelet, its configuration and the sysext stay. The NodeClass is not `Ready`
(`ValidationSucceeded=False`, `TemplateInvalid`) while the template does not
render, so a typo does not reach a node; `kubectl describe scnodeclass` shows the
error.
