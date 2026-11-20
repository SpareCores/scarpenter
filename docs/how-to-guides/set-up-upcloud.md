# Set up UpCloud

The UpCloud driver creates cloud servers by plan name in UpCloud zones (UpCloud's regions are its
zones), cloned from a Flatcar template you import into your account. It writes the Public & Utility
firewall of every server and can create SDN private networks, firewall rulesets and server groups.

**Preview.** The driver is implemented and covered by unit and offline tests, but it has not run against the real cloud yet: expect rough edges and report them.

## What you need

- **An UpCloud account** with a dedicated **sub-account** for SCarpenter ([credentials](#2-credentials)).
  There are no projects: every limit is account-wide.
- **Resource limits** on the sub-account (cores, memory, storage per tier, public IPv4, networks), set
  by the main account, and a spending alert. A token has no scopes and cannot deny a plan or a zone,
  so these limits are the only bound on what SCarpenter can spend.
- **Reachability.** Nodes get a public IPv4 address by default and need it for egress: they must reach
  `cluster.apiServerEndpoint` and the artifact hosts (the Flatcar release server and the sysext-bakery
  releases on GitHub, or your mirrors). The driver's firewall drops all unsolicited inbound traffic, so
  the API server cannot reach a node's kubelet over the public address. Use the `tailscale` network
  template when the control plane is outside UpCloud; with `direct`, add a `spec.networking.inbound`
  rule for TCP 10250 from the control plane's addresses ([network templates](choose-a-network-template.md)).
- The controller needs HTTPS to `api.upcloud.com` (if the token is limited to source addresses,
  include the controller's egress address).

## 1. The Flatcar image

UpCloud has no public Flatcar template and templates cannot be shared between accounts, so every node is
a clone of a **template in your account**: one per zone you use and per storage tier you boot from.
SCarpenter never imports; it finds the templates by their labels and reports them. A clone keeps its
template's tier: import MaxIOPS everywhere, and Standard too where you use Starter plans (their included
disk is Standard) or set `storage.root.tier: standard`. With a Standard template, a Starter node clones it (the
disk the plan's price includes), which takes minutes; without one it clones the MaxIOPS template.

The image is the Spare Cores images repository's release asset
`flatcar-upcloud-<channel>-<version>-amd64.img.gz` of the release you pin in `spec.flatcar.version`
(raw, gzip, OEM id `upcloud`). Channels `stable`, `beta` and `alpha` only (the NodeClass refuses `lts`).

For every zone and tier, in the control panel, the API or `upctl` (check its flags with `upctl help`):

1. Create a storage of 10 GB in the zone and tier.
2. Import the `.img.gz` into it: a direct upload, or an HTTP import of its URL (served as
   `application/gzip`), with the SHA-256 as the source hash. The import decompresses it.
3. Make a template of the storage ("templatize"); title it `flatcar-<channel>-<version>-amd64-<tier>`.
4. Label the template `scarpenter-image=flatcar`, `flatcar-channel=<channel>`,
   `flatcar-version=<version>` (for example `4757.2.3`), `flatcar-arch=amd64`, and
   `flatcar-tier=maxiops|standard` (informational: the storage's own tier counts).
5. Delete the source storage; it is billed until you do. The template is billed as storage of its tier.

The driver boots the `online` template whose labels match the pinned channel and version. To name
templates instead, set `vendors.upcloud.resources.images.use` (both tiers may go in one list, see
[the bring-your-own example](../examples/upcloud-bring-your-own.yaml)); the chosen template's labels must
still say the pinned release.

In an account shared with other users of SCarpenter (for example a CI lane that labels its templates for
itself), do not rely on the generic `scarpenter-use=true` label: select your production templates by a label
specific to you, such as `resources.images.use.tags: {my-team-image: prod, flatcar-version: <version>}`, so
that templates of others never match your selector.

To upgrade Flatcar, import the new release's templates everywhere first, then change
`spec.flatcar.version` (the nodes drift); keep the old templates until no NodeClass pins them.

## 2. Credentials

UpCloud has no workload identity and no OIDC federation: the only mode is an **API token** in
`UPCLOUD_TOKEN`. The controller does not start without it.

1. In the main account, create a sub-account with API access and permission for servers, storage,
   networks and firewalls, no control panel access, and the resource limits above. It must see the
   templates, and any networks or rulesets you bring.
2. Create an API token for the sub-account. Tokens expire (a sub-account's last up to a year): note
   the date.
3. Store it in a Secret in the controller's namespace:

   ```bash
   kubectl -n scarpenter-system create secret generic upcloud-credentials \
     --from-literal=UPCLOUD_TOKEN=<token>
   ```

To rotate, create a new token, update the Secret, restart the controller
(`kubectl -n scarpenter-system rollout restart deployment/scarpenter`) and delete the old token.
The calls the driver makes are under [permissions](../reference/upcloud-driver.md#permissions).
Alternative: `credentials.mode: none` wires nothing; set `UPCLOUD_TOKEN` yourself with `extraEnv`.

## 3. Enable the driver

```yaml
vendors:
  upcloud:
    enabled: true
    credentials:
      mode: secret                      # the default
      secret:
        existingSecret: upcloud-credentials
        tokenKey: UPCLOUD_TOKEN         # the default
    networkRange: ""                    # the /16 of managed SDN networks; empty: 10.241.0.0/16
```

`networkRange` must not overlap a network you bring (an account's networks must not overlap). Apply
with `helm upgrade --install` ([install](install.md)); `upcloud` joins the controller's `--vendors`.

## 4. A first NodeClass and NodePool

[`upcloud-minimal.yaml`](../examples/upcloud-minimal.yaml) finds the templates by their labels; this one
uses the `tailscale` template ([network templates](choose-a-network-template.md#with-tailscale)):

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata:
  name: upcloud
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network:
    template: tailscale
    tailscale:
      version: "v1.98.10"
      authKeySecretRef: {name: ts-node-key, namespace: scarpenter-system, key: authkey}
      tags: ["tag:k8s-node"]
  vendors:
    upcloud:
      regions: [fi-hel1]
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: upcloud
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: upcloud}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [upcloud]}
  limits: {cpu: "16"}
```

Under the default `networking.isolation: run` (without a run key) SCarpenter creates an SDN network and
ruleset per zone so that the nodes of the NodeClass reach each other privately; `isolation: none` skips
them. A placement profile with a server group per run key needs `isolation: nodeclass` or `none`.
[`upcloud-bring-your-own.yaml`](../examples/upcloud-bring-your-own.yaml) brings templates, a network and a ruleset.

## 5. Check that it works

```bash
kubectl get scnodeclass upcloud        # Ready=True
kubectl describe scnodeclass upcloud   # the conditions below, and status.vendors.upcloud.images
```

The conditions of `status.vendors.upcloud` that matter: `ImagesReady` (a template in every zone),
`ResourcesReady` (networks, rulesets, server groups), `QuotaAvailable` (the account's limits). Then
run a Pod that selects the pool:

```bash
kubectl run upcloud-test --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"upcloud"}}}'
kubectl get nodeclaims -w
```

A server is cloned from the template first, so a node takes a few minutes. Launch errors show on the
NodeClaim (`kubectl describe nodeclaim`), in the controller's events and in its logs
(`kubectl -n scarpenter-system logs deployment/scarpenter`).

## Limits

- amd64 only; on-demand only (no spot); GPU plans are not launched.
- No local disks: every disk is network storage. At most four storage devices per server (root and three data disks).
- `isolation: run` with a run key is not supported (no SDN network per run).
- `publicIPv4: false` needs a brought private network with a NAT gateway or router.
- Billing retention stays off: the billing rule is not verified.
- Details and every unverified fact: [UpCloud driver](../reference/upcloud-driver.md).

## Troubleshooting

| Symptom | Fix |
|---|---|
| The controller exits with "initialising the upcloud driver: ... UPCLOUD_TOKEN is not set" | Check `existingSecret` and `tokenKey`, or `extraEnv` with mode `none`. |
| Calls fail with `AUTHENTICATION_FAILED` | The token expired or was deleted: rotate it (above). |
| `ImagesReady=False`, "import the Flatcar template for <version>" | No `online` template in that zone labelled `scarpenter-image`, `flatcar-channel`, `flatcar-version` and `flatcar-arch` as above: import or label it. |
| `ImageVersionMismatch` or `ImageVersionUnknown` | The template's `flatcar-version` label differs from the pin, or is missing. |
| A launch is `Unsupported` naming a tier | No template of the root disk's tier in the zone: import one, or set `storage.root.tier`. |
| `QuotaAvailable=False`, or `SERVER_CORE_LIMIT_REACHED` and similar | The sub-account's resource limits: raise them in the main account. |
| `NETWORK_LIMIT_REACHED` | Too many networks in the account: raise the limit or set `isolation: none`. |
| The node never joins | The server's console in the control panel; with `direct`, the inbound rule for the kubelet. |
| Pods on a new node stay in `ContainerCreating`, "failed to pull image registry.k8s.io/pause" | `registry.k8s.io` refused the node's address (seen in the CI lane). Pull through its Artifact Registry with `spec.butane`: a containerd drop-in and `hosts.toml` as in `test/e2e-upcloud/manifests/node-butane.yaml`. |
