# Controller reference

The SCarpenter controller is one binary (`scarpenter-controller`) that runs
Karpenter core's operator with SCarpenter's CloudProvider. This page lists what
the controller accepts and reports: flags and environment variables, the
[Helm values](#helm-values) that set them, conditions, Events, metrics and
[permissions](#permissions). `scarpenter-controller --version` prints the version
and commit of the build and exits.

## Flags and environment variables

Every flag has a `SCARPENTER_*` environment fallback; a flag on the command line
wins. Karpenter core's own flags and feature gates (`--log-level`,
`--metrics-port`, `--feature-gates`, ...) are accepted on the same command line
with their upstream names and environment variables (`LOG_LEVEL`,
`METRICS_PORT`, ...); see the
[Karpenter documentation](https://karpenter.sh/docs/reference/settings/).

The last column names the plan that introduced the flag.

| Flag | Environment | Default | Meaning | Used from |
|---|---|---|---|---|
| `--cluster-name` | `SCARPENTER_CLUSTER_NAME` | required | Tag value on vendor resources and part of their names. Letters, digits, `-` and `_`, at most 63 characters, starting with a letter or digit. | plan 04 |
| `--api-server-endpoint` | `SCARPENTER_API_SERVER_ENDPOINT` | required | `https://` URL nodes use to reach the API server. | plan 05 |
| `--tailnet-prefixes` | `SCARPENTER_TAILNET_PREFIXES` | empty (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) | Comma-separated CIDRs of the tailnet. With the `tailscale` template a kubelet serving certificate may name a node address only inside them (plus the instance's own addresses); set it for a Headscale with other prefixes. | plan 08 |
| `--cluster-dns` | `SCARPENTER_CLUSTER_DNS` | empty | Comma-separated IPs of the cluster DNS service, set as the kubelet's `clusterDNS` on nodes. **Empty: the controller reads the ClusterIPs of the DNS Service of `kube-system` itself** (the Service labelled `k8s-app=kube-dns`, else the one named `kube-dns`; cached for 5 minutes), and an SCNodeClass is `ValidationSucceeded=False` with reason `ClusterDNSUnknown`, naming this flag and the Helm value, while it finds none. `none` leaves `clusterDNS` unset on purpose: Pods then use the node's resolver. | plan 05, 09 |
| `--cluster-domain` | `SCARPENTER_CLUSTER_DOMAIN` | `cluster.local` | The kubelet's `clusterDomain` on nodes. | plan 05 |
| `--cluster-ca-file` | `SCARPENTER_CLUSTER_CA_FILE` | empty | File with the cluster CA bundle given to nodes. Empty means the `kube-root-ca.crt` ConfigMap of the controller's namespace (`POD_NAMESPACE`; `kube-system` without it). | plan 05 |
| `--kubelet-client-ca-file` | `SCARPENTER_KUBELET_CLIENT_CA_FILE` | empty | File with the CA bundle that signs the API server's kubelet client certificates, for control planes where it is not the cluster CA (k3s: `<data-dir>/server/tls/client-ca.crt`). Written to the nodes as `/etc/kubernetes/pki/kubelet-client-ca.crt` and used as the kubelet's `authentication.x509.clientCAFile`; without it the kubelet trusts the cluster CA, and `kubectl logs` and `exec` fail with 401 on k3s. Read at every rendering. | plan 07 |
| `--vendors` | `SCARPENTER_VENDORS` | `aws` | Comma-separated list of enabled vendor drivers. | plan 04 |
| `--keeper-url` | `SCARPENTER_KEEPER_URL` | `https://keeper.sparecores.net` | Base URL of the Keeper API: the public Spare Cores one by default, or a self-hosted Keeper (open source, <https://github.com/SpareCores/sc-keeper>). | plan 03 |
| `--keeper-api-key-file` | `SCARPENTER_KEEPER_API_KEY_FILE` | empty | File with the Keeper API key. The public Keeper requires one ([Keeper API key](../how-to-guides/install.md#keeper-api-key)); optional for a self-hosted Keeper. Read at every catalog refresh, so a rotated Secret takes effect without a restart; an empty file is an error. (The development `fake` driver reads the key once, at start.) | plan 03, plan 13 |
| `--keeper-cache-dir` | `SCARPENTER_KEEPER_CACHE_DIR` | empty (no copy) | Directory that keeps Keeper's snapshot files of the pinned data version (`<dir>/<hash>/...`, the gzip bytes as received, checked against the manifest's SHA-256 before use). A restart within the same data version reads only `/healthcheck` from Keeper; a new version fetches only the files that changed. Directories of other versions are deleted after a successful refresh. The chart sets it to an `emptyDir` at `/var/cache/scarpenter/keeper`. | plan 13 |
| `--catalog-refresh-interval` | `SCARPENTER_CATALOG_REFRESH_INTERVAL` | `1h` | How often the catalog is refreshed (with 10 % jitter). | plan 03 |
| `--catalog-max-age` | `SCARPENTER_CATALOG_MAX_AGE` | `24h` | Snapshot age after which SCNodeClasses report `CatalogReady=False`. Must not be shorter than the refresh interval. | plan 03 |
| `--sysext-base-url` | `SCARPENTER_SYSEXT_BASE_URL` | empty (upstream) | Mirror of the Flatcar sysext-bakery releases: `<base>/<name>-<version>/...` as upstream. | plan 05 |
| `--flatcar-base-url` | `SCARPENTER_FLATCAR_BASE_URL` | empty (upstream) | Mirror of the Flatcar release files (`<base>/<arch>-usr/<version>/...`; `{channel}` in the base is replaced by the channel). | plan 05 |
| `--nvidia-driver-base-url` | `SCARPENTER_NVIDIA_DRIVER_BASE_URL` | empty (Flatcar's release server) | Mirror of the NVIDIA driver sysexts of the Flatcar releases: `<base>/amd64-usr/<version>/flatcar-nvidia-drivers-<branch>[-open].raw.gz`, built by `hack/nvidia-mirror.sh`. When set, GPU nodes get the image through Ignition (pinned by sha256) and download nothing from Flatcar's server. See [NVIDIA GPU nodes](node-bootstrap.md#nvidia-gpu-nodes). | plan 12 |
| `--bootstrap-token-ttl` | `SCARPENTER_BOOTSTRAP_TOKEN_TTL` | `1h` | Lifetime of a node's bootstrap token; at least `20m`, because core gives a NodeClaim 15 minutes to register and the token must outlive that. | plan 05 |
| `--tailnet-backend` | `SCARPENTER_TAILNET_BACKEND` | `none` | Who mints per-node tailnet auth keys (`network.tailscale.keySource: PerNode`): `none` (such NodeClasses are not valid), `tailscale` (Tailscale SaaS) or `headscale` (self-hosted, 0.28 or newer). One backend per controller; a NodeClass's `loginServer` must match it ([Per-node tailnet keys](node-bootstrap.md#per-node-tailnet-keys)). | plan 27 |
| `--tailnet-api-url` | `SCARPENTER_TAILNET_API_URL` | `https://api.tailscale.com` with `tailscale`; required with `headscale` | Base URL of the backend's API. Headscale: the server URL, which the NodeClasses' `loginServer` must equal (scheme, host, port and path; a trailing slash is ignored; no user information, query or fragment). | plan 27 |
| `--tailnet-credentials-dir` | `SCARPENTER_TAILNET_CREDENTIALS_DIR` | empty | Directory with one file per key of the credentials Secret: `client-id` and `client-secret` (a Tailscale OAuth client), or `api-key` (a Headscale API key). Read at every call, so a rotated Secret takes effect without a restart. Without it a `PerNode` NodeClass is not valid. | plan 27 |
| `--tailnet-key-ttl` | `SCARPENTER_TAILNET_KEY_TTL` | `1h` | Expiry of a minted per-node key; at least `30m`, because core gives a NodeClaim 15 minutes to register and retries come within it. | plan 27 |
| `--max-launch-attempts` | `SCARPENTER_MAX_LAUNCH_ATTEMPTS` | `3` | Vendor calls one NodeClaim creation may make; candidates skipped for lack of quota headroom do not count. | plan 04 |
| `--quota-refresh-interval` | `SCARPENTER_QUOTA_REFRESH_INTERVAL` | `2m` | How often quota usage is refreshed per vendor and region in scope. | plan 04 |
| `--quota-limits-refresh-interval` | `SCARPENTER_QUOTA_LIMITS_REFRESH_INTERVAL` | `15m` | How often quota limits are refreshed. | plan 04 |
| `--quota-max-staleness` | `SCARPENTER_QUOTA_MAX_STALENESS` | `15m` | Age after which a region whose quota refreshes fail counts as unknown quota. | plan 04 |
| `--quota-launchable-refresh-interval` | `SCARPENTER_QUOTA_LAUNCHABLE_REFRESH_INTERVAL` | `1h` | How often the instance types the account can launch per zone (`OfferingLister`) are refreshed; the offered listing waits for the first refresh of every region (at most 5 minutes for a failing one). | plan 04 |
| `--gc-interval` | `SCARPENTER_GC_INTERVAL` | `2m` | How often the garbage collector looks for orphans. | plan 04 |
| `--gc-grace-period` | `SCARPENTER_GC_GRACE_PERIOD` | `5m` | Age an instance (or bootstrap token) without a NodeClaim must have before it is deleted. | plan 04 |
| `--terminating-grace` | `SCARPENTER_TERMINATING_GRACE` | `10m` | How long an instance may be seen `terminating` before it is deleted again, by the garbage collector (no NodeClaim) and by `Delete`. The first sighting is kept in memory, so a restart restarts the clock. A stopped or powered-off instance is `terminating` for ever; `Delete` deletes it at once and waits the grace only after its own accepted delete; one the vendor is deleting disappears well inside the grace. | plan 14 |
| `--gc-dry-run` | `SCARPENTER_GC_DRY_RUN` | `false` | Log (and count, `result="dry-run"`) what the garbage collector would delete without deleting it. | plan 04 |
| `--metrics-offering-detail` | `SCARPENTER_METRICS_OFFERING_DETAIL` | `false` | Export `scarpenter_instance_type_offering_available`, one gauge per offering of every NodeClass (thousands of series per class and vendor). | plan 28 |
| `--billing-retention` | `SCARPENTER_BILLING_RETENTION` | `true` | Run the [billing retention controller](#billing-retention). The NodeClass field `spec.billing.retainUntilPeriodEnd` is the real switch (off by default); `false` turns the whole controller off. | plan 23 |
| `--interruption` | `SCARPENTER_INTERRUPTION` | `true` | Run the [interruption controller](#interruption). `false` turns it off. | plan 26 |
| `--interruption-margin` | `SCARPENTER_INTERRUPTION_MARGIN` | `5s` | How long before the notice's deadline the Pods of an interrupted node are force-deleted: the time the instance needs after the kubelet stopped the containers. | plan 26 |
| `--interruption-unavailable-ttl` | `SCARPENTER_INTERRUPTION_UNAVAILABLE_TTL` | `10m` | How long an interrupted spot offering (instance type and zone) stays unavailable. | plan 26 |
| `--unavailable-insufficient-capacity-ttl` | `SCARPENTER_UNAVAILABLE_INSUFFICIENT_CAPACITY_TTL` | `0` (3m) | How long an offering stays unavailable after `InsufficientCapacity`. | plan 04 |
| `--unavailable-unsupported-ttl` | `SCARPENTER_UNAVAILABLE_UNSUPPORTED_TTL` | `0` (24h) | How long a type stays unavailable in a zone after `Unsupported`. | plan 04 |
| `--unavailable-quota-exceeded-ttl` | `SCARPENTER_UNAVAILABLE_QUOTA_EXCEEDED_TTL` | `0` (5m) | How long a quota bucket stays unavailable after `QuotaExceeded` (the region and capacity type for a vendor without a bucket mapping; every region of the vendor when the error is account-wide). A NodeClaim of the vendor and region confirmed gone clears it. | plan 04 |
| `--unavailable-zone-unusable-ttl` | `SCARPENTER_UNAVAILABLE_ZONE_UNUSABLE_TTL` | `0` (10m) | How long a zone stays unavailable after `ZoneUnusable`. | plan 04 |
| `--resource-default-ttl` | `SCARPENTER_RESOURCE_DEFAULT_TTL` | `24h` | `idleTTL` of managed shared resources when an SCNodeClass does not set one. | plan 06 |
| `--image-copy-idle-ttl` | `SCARPENTER_IMAGE_COPY_IDLE_TTL` | `720h` | How long a boot image copy SCarpenter made into a region ([ADR-032](../explanation/DECISIONS.md#adr-032-scarpenter-copies-boot-images-it-can-already-read-into-regions-of-the-account-opt-in-accepted-2026-11-23)) may go unused before the garbage collector deletes it; at least `1m`. No effect until a vendor's copy flag is on. Chart value `imageCopies.idleTTL`. [Image copies](image-copies.md). | plan 29 |
| `--aws-copy-images` | `SCARPENTER_AWS_COPY_IMAGES` | `false` | Copy the Flatcar image of the pinned release into the AWS regions that have none, when a launch wants the region, and delete the copies nothing uses. Refuses to start when the `aws` vendor is not enabled. Needs the optional image copy statements of the [AWS policy](aws-iam-policy.json). Chart value `vendors.aws.imageCopies.enabled`. [Image copies](image-copies.md). | plan 29 |
| `--alicloud-copy-images` | `SCARPENTER_ALICLOUD_COPY_IMAGES` | `false` | The same for Alibaba Cloud (until its driver supports copies, plan 29 step 5, the controller refuses to start with it on). Chart value `vendors.alicloud.imageCopies.enabled`. | plan 29 |
| `--resource-per-use-grace` | `SCARPENTER_RESOURCE_PER_USE_GRACE` | `2m` | How long an unused `perUse` managed resource is kept for launches in flight. | plan 06 |
| `--resource-full-sweep-every` | `SCARPENTER_RESOURCE_FULL_SWEEP_EVERY` | `10` | Every this many garbage collection sweeps (and after a NodeClass was deleted) the managed resources of every region of the driver are swept, not only of the regions in use. **Cost:** a full sweep asks the vendor, for every region the account can use, which managed resources exist (for AWS: a few `Describe*` calls per resource kind and region): read-only and free, but rate limited, and it runs on the leader only. At the default `--gc-interval` of 2m it happens every 20 minutes; raise the value to make it rarer, `1` makes every sweep a full one. | plan 06 |
| `--resource-check-interval` | `SCARPENTER_RESOURCE_CHECK_INTERVAL` | `30s` | How often the vendor resources of each SCNodeClass are checked (`status.vendors.<vendor>.resources`, `ResourcesReady`, preCreate). | plan 06 |
| (AWS only) | `SCARPENTER_AWS_IPV4_PRICE` | unset (Keeper's) | Price of a public IPv4 address per hour in USD, used in the price delta of AWS offerings. Unset (the default), the price is Keeper's `ipv4_price` row of the region, and a region without a row makes the price delta incomplete; set, it is an explicit override that wins over Keeper. `0` means free; negative, NaN and infinite values are refused at startup. The driver logs the value it uses. Environment only. | plan 08 |
| `--gcp-project` | `SCARPENTER_GCP_PROJECT` | project of the credentials | The Google Cloud project of the GCP driver's nodes: the controller targets one. Required with workload identity federation (it carries no project); a NodeClass's `vendors.gcp.project`, when set, must equal it ([GCP driver](gcp-driver.md#credentials-and-clients)). | plan 16 |
| (GCP only) | `SCARPENTER_GCP_IPV4_PRICE` | unset (Keeper's) | Price of an external IPv4 address per hour in USD, used in the price delta of GCP offerings. Unset, Keeper's `ipv4_price` row of the region (none: incomplete delta); set, an explicit override that wins over Keeper; `0` means free; negative, NaN and infinite values are refused at startup. Environment only. | plan 16 |
| `--gcp-preemption-notice` | `SCARPENTER_GCP_PREEMPTION_NOTICE` | `0` | The preemption notice a GCP Spot VM is launched with (`scheduling.preemptionNoticeDuration`), 0 to `2m` in whole seconds. `0` asks for none: Pods get the 30 s of the ACPI shutdown. Chart: `vendors.gcp.preemptionNotice`. Whether the notice costs extra is unverified ([GCP interruption](gcp-driver.md#interruption)). | plan 26 |
| `--oci-compartment` | `SCARPENTER_OCI_COMPARTMENT` | none | The OCID of the OCI compartment of the nodes (**required** with `oci` in `--vendors`): instances, boot volumes, the managed network and the default image lookup live in it; the tenancy root is not a default. One compartment for every NodeClass ([OCI credentials](../how-to-guides/set-up-oci.md#2-credentials)). | plan 25 |
| `--oci-image-compartment` | `SCARPENTER_OCI_IMAGE_COMPARTMENT` | none (the `--oci-compartment`) | The OCID of the compartment the OCI driver lists images in: the default image lookup and the `images.use` selectors (name, tags, operating system). Empty: `--oci-compartment`. `images.use.ids` is not affected and launches stay in `--oci-compartment`; the identity needs `read instance-images` there ([OCI images](../how-to-guides/set-up-oci.md#1-the-flatcar-image)). | plan 25 |
| `--oci-interruption-poll` | `SCARPENTER_OCI_INTERRUPTION_POLL` | `10s` | How often the OCI driver lists a region's instances to find preempted ones (at least `1s`): one paged `ListInstances` call per region per interval, whatever the number of nodes ([OCI interruption](oci-driver.md#interruption)). Chart: `vendors.oci.interruptionPoll`. | plan 26 |
| `--oci-auth` | `SCARPENTER_OCI_AUTH` | `apiKey` | The OCI credentials mode: `apiKey` or `workloadIdentityFederation`. `instancePrincipal` and `okeWorkloadIdentity` are not supported yet and are refused with the supported modes named. | plan 25 |
| `--oci-credentials-dir` | `SCARPENTER_OCI_CREDENTIALS_DIR` | none | `apiKey`: a directory with one file per key of the API-key Secret (`tenancy`, `user`, `fingerprint`, `region`, `key`, optional `passphrase`). Wins over `--oci-config-file`. | plan 25 |
| `--oci-config-file`, `--oci-config-profile` | `SCARPENTER_OCI_CONFIG_FILE`, `SCARPENTER_OCI_CONFIG_PROFILE` | none, `DEFAULT` | `apiKey`: an OCI config file and its profile (the development path; a profile with `security_token_file` is a session-token profile). Without it and the directory, the `OCI_CLI_*` variables are read. | plan 25 |
| `--oci-region`, `--oci-token-file`, `--oci-identity-domain-url`, `--oci-client-id`, `--oci-client-secret-file` | `SCARPENTER_OCI_REGION`, `SCARPENTER_OCI_TOKEN_FILE`, `SCARPENTER_OCI_IDENTITY_DOMAIN_URL`, `SCARPENTER_OCI_CLIENT_ID`, `SCARPENTER_OCI_CLIENT_SECRET_FILE` | none | `workloadIdentityFederation` (all five required): the tenancy's home region, the file with the token to exchange (read at every exchange), the identity domain's URL and the confidential application's client ID and client secret file. | plan 25 |
| `--hcloud-server-limit` | `SCARPENTER_HCLOUD_SERVER_LIMIT` | `0` (unknown) | The server limit of the Hetzner Cloud project, as the Console shows it (the API does not report it). With it the driver reports one global quota bucket and headroom is known before a launch; without it only the API's own refusal ends a launch. Negative values are refused. | plan 15 |
| `--hcloud-build-images` | `SCARPENTER_HCLOUD_BUILD_IMAGES` | `true` | Let the hcloud driver build the Flatcar snapshot of the pinned release when the project has none (ADR-030, [Hetzner images](hcloud-driver.md#images)). Only for NodeClasses without `resources.images.use`. `false` for operators who pre-create the snapshots. Chart: `vendors.hcloud.buildImages`. | ADR-030 |
| (hcloud only) | `SCARPENTER_HCLOUD_IPV4_PRICE` | unset (Keeper's) | Price of a Primary IPv4 address per hour in USD, used in the price delta of Hetzner offerings. Unset, Keeper's `ipv4_price` row of the region (a monthly price divided by 730 hours; none: incomplete delta); set, an explicit override that wins over Keeper. `0` means free; negative, NaN and infinite values are refused at startup. Environment only. | plan 15 |
| (OVH only) | `SCARPENTER_OVH_SCHEDULE_WAIT` | `45s` | How long a server create waits for Nova's scheduler (a Go duration), so that a capacity failure shows as the create's error and the attempt loop tries the next candidate; a server still scheduling after it is returned as pending. Environment only ([OVHcloud driver](ovh-driver.md)). | plan 18 |
| (OVH only) | `SCARPENTER_OVH_REGION_ALIASES` | (none) | Rows of the OVH region table: comma-separated `KEEPER=API` pairs (`GRA13=GRA13` adds a region Keeper knows and the table lacks); an empty API name leaves the Keeper region unused. A row whose API name differs from the Keeper name is refused at start: it needs the Keeper name in `vendors.Region` (plan 14, A8: a core change not made yet), without which it would hide the region's offerings. Environment only. | plan 18 |
| (Vultr only) | `VULTR_API_KEY` | (none) | The API key of the Vultr account. Required when `--vendors` has `vultr`: the controller refuses to start without it. Environment only; the chart wires it from a Secret. [Vultr credentials](../how-to-guides/set-up-vultr.md#2-credentials). | plan 20 |
| (Vultr only) | `SCARPENTER_VULTR_AVAILABILITY_TTL` | `5m` | How long the stock of a Vultr region (`GET /v2/regions/{id}/availability`) is cached by the driver. Environment only. | plan 20 |
| (UpCloud only) | `SCARPENTER_UPCLOUD_NETWORK_RANGE` | `10.241.0.0/16` | The IPv4 /16 inside which the UpCloud driver's managed private networks take a /24 each (hash of the name, the next free one when it overlaps a network of the account). It must stay clear of the networks you bring: the networks of an UpCloud account must not overlap. A value that is not an IPv4 /16 is refused at startup. Environment only. | plan 19 |
| (Azure only) | `AZURE_SUBSCRIPTION_ID` | required | The subscription the Azure driver works in: one controller, one subscription, no discovery. Environment only (no flag: the Azure SDK's own variable). | plan 17 |
| (Azure only) | `SCARPENTER_AZURE_CREDENTIALS_MODE` | `workloadIdentity` | `workloadIdentity` (also `none`) or `secret`: the credential chain, see [Azure credentials](../how-to-guides/set-up-azure.md#2-credentials). Environment only. | plan 17 |
| (Azure only) | `SCARPENTER_AZURE_DEVELOPMENT` | `false` | `true` ends the chain in the Azure CLI's login (the sweeper and a developer's machine; never in a cluster). Environment only. | plan 17 |
| (Azure only) | `SCARPENTER_AZURE_IPV4_PRICE` | unset (Keeper's) | Price of a Standard public IPv4 address per hour in USD, used in the price delta of Azure offerings. Unset, Keeper's `ipv4_price` row of the region (none: incomplete delta); set, an explicit override that wins over Keeper; `0` means free; invalid values are refused at startup. Environment only. | plan 17 |
| `--aws-home-region` | `SCARPENTER_AWS_HOME_REGION` | `us-east-1` | AWS region the account's region list (`DescribeRegions`) is read from; the AWS driver's other calls go to the region they concern ([AWS driver](aws-driver.md)). | plan 08 |

All problems of the command line are reported together, and the controller
refuses to start on any of them. Durations are Go durations (`90s`, `2h30m`) and
must be positive.

### Other environment variables

| Variable | Meaning |
|---|---|
| `POD_NAMESPACE` | The controller's namespace: where `kube-root-ca.crt` is read when `--cluster-ca-file` is empty (`kube-system` without it) and the namespace of the Deployment its Events go to. The chart sets it from the Pod's namespace. |
| `SCARPENTER_DEPLOYMENT_NAME` | The Deployment (in `POD_NAMESPACE`) that the Events of the garbage collector are attached to when an orphan has no NodeClass. The chart sets it to the release's full name. |
| `LEADER_ELECTION_NAMESPACE`, `LEADER_ELECTION_NAME` | Namespace and name of core's leader election lease (default name `karpenter-leader-election`). The chart sets the namespace from the Pod's namespace, the name to `<release>-leader-election` (so that another Karpenter in the same namespace does not share the lease) and gives the controller a Role for it. |
| `MEMORY_LIMIT`, `CPU_REQUESTS` | Core's: the container's memory limit in bytes (the garbage collector's soft limit is 90 % of it) and CPU request in millicores (sizes core's concurrency). The chart derives both from the container's resources. |
| `DISABLE_LEADER_ELECTION` | Core's: `true` runs without leader election; the chart sets it with `leaderElection.enabled=false`. |
| `SCARPENTER_ALICLOUD_HOME_REGION` | The region the Alibaba Cloud account's region list (`DescribeRegions`) is read from; default `ap-southeast-1`. An environment variable of the driver, not a flag ([Alibaba Cloud driver](alicloud-driver.md)). |
| `SCARPENTER_ALICLOUD_CALL_TIMEOUT` | The deadline of one Alibaba Cloud API call, a Go duration; default `45s` (`1m` and the like). It does not help a regional endpoint that cannot be reached: the clients connect within 10 s or fail. The zone load of the facts follows it ([Alibaba Cloud driver](alicloud-driver.md)). An environment variable of the driver, not a flag. |
| `ALIBABA_CLOUD_ROLE_ARN`, `ALIBABA_CLOUD_OIDC_PROVIDER_ARN`, `ALIBABA_CLOUD_OIDC_TOKEN_FILE`, `ALIBABA_CLOUD_ACCESS_KEY_ID`, ... | The Alibaba Cloud SDK's own: the [credential modes](../how-to-guides/set-up-alicloud.md#2-credentials) of the chart set them. |
| `OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`, `OS_PROJECT_ID`, `OS_USER_DOMAIN_NAME` | The OpenStack environment of the OVH driver (gophercloud's, plus `OS_USER_DOMAIN_NAME` or `OS_USER_DOMAIN_ID` for the user's domain when `OS_DOMAIN_NAME` and `OS_DOMAIN_ID` are unset): one dedicated user of one Public Cloud project; the chart sets them from a Secret ([OVH credentials](../how-to-guides/set-up-ovh.md#2-credentials)). `Init` checks them and makes no call. |
| `UPCLOUD_TOKEN` | The UpCloud API token (a bearer token) of the [UpCloud driver](upcloud-driver.md#credentials); the chart's `vendors.upcloud.credentials.mode: secret` sets it from a Secret. The driver does not start without it. |
| `GOOGLE_APPLICATION_CREDENTIALS` | The Google client libraries' own: the [credential modes](../how-to-guides/set-up-gcp.md#2-credentials) of the chart set it (workload identity federation: the credential configuration of a ConfigMap; secret: a service account key). Without it Application Default Credentials find the metadata server. |
| `AWS_REGION`, `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ACCESS_KEY_ID`, ... | The AWS SDK's own: the [credential modes](../how-to-guides/set-up-aws.md#2-credentials) of the chart set them. Without `AWS_REGION` the driver uses `--aws-home-region` as the SDK's default region. |
| `OCI_CLI_TENANCY`, `OCI_CLI_USER`, `OCI_CLI_FINGERPRINT`, `OCI_CLI_REGION`, `OCI_CLI_KEY_FILE` or `OCI_CLI_KEY_CONTENT`, `OCI_CLI_PASSPHRASE` | The OCI API key of the `apiKey` mode when neither `--oci-credentials-dir` nor `--oci-config-file` is set (the chart's `vendors.oci.credentials.mode: none` with `extraEnv`). See [OCI credentials](../how-to-guides/set-up-oci.md#2-credentials). |
| `HCLOUD_TOKEN`, `HCLOUD_ENDPOINT` | The Hetzner Cloud driver's: the API token of the project (a Secret; the chart's `vendors.hcloud.credentials.secret.existingSecret`) and, for tests, the API URL. Required when `hcloud` is in `--vendors`. See the [Hetzner Cloud driver](hcloud-driver.md). |

Core's other flags and environment variables (`--log-level`/`LOG_LEVEL`,
`--metrics-port`/`METRICS_PORT` (8080), `--health-probe-port`/`HEALTH_PROBE_PORT`
(8081), `--batch-max-duration`, `--batch-idle-duration`, `--registration-timeout`,
`--feature-gates`, ...) are those of Karpenter core v1.15 (patched, see below); the
chart has values for the ones above and `extraArgs` for the rest.

`--registration-timeout` / `REGISTRATION_TIMEOUT` (chart: `karpenter.registrationTimeout`)
is how long a NodeClaim may take to register as a Node before core's liveness
check deletes it. Default `15m` (the chart passes the flag only when the value
is set); it must be greater than zero. Raise it for instances that need longer
to boot and join: Vultr bare metal measured about 15m10s to register and
15m41s to be Ready, so use `25m` or more: the Vultr driver offers bare-metal (`vbm`) plans only from `25m` up (the controller
passes the value to the drivers), and a bootstrap token lives at least the timeout plus 5 minutes whatever
`--bootstrap-token-ttl` says. The option exists only in SCarpenter's
patched core: SCarpenter builds against the
[SpareCores/karpenter](https://github.com/SpareCores/karpenter) fork, which adds
it to Karpenter v1.15.1 ([ADR-033](../explanation/DECISIONS.md#adr-033-a-patched-karpenter-core-with-a-configurable-registration-timeout-accepted-2026-11-24)). The controller serves `/healthz` and `/readyz` on the
health port and Prometheus metrics on the metrics port (plain HTTP).

## Helm values

The chart is `charts/scarpenter` (installed from `oci://ghcr.io/sparecores/charts/scarpenter`,
[install guide](../how-to-guides/install.md)); `values.yaml` documents every value
and `values.schema.json` rejects unknown keys and malformed values. A value that is
empty or `null` leaves its flag out, so the controller's default above applies.
`test/chart` checks that every flag of the table above can be set from the chart
and that the controller's own parser accepts what the chart renders.

| Value | Flag or environment | Notes |
|---|---|---|
| `cluster.name` | `--cluster-name` | Required. |
| `cluster.apiServerEndpoint` | `--api-server-endpoint` | Required, `https://`. |
| `cluster.ca.existingConfigMap`, `existingSecret`, `key` | `--cluster-ca-file` | Mounts the object (of the release namespace) and points the flag at the key. Unset: the `kube-root-ca.crt` ConfigMap. |
| `cluster.dns` | `--cluster-dns` | A list of IPs, passed as it is. **Empty (the default): the controller finds the DNS Service itself** (the chart looks nothing up); `[none]` gives the nodes no `clusterDNS`. |
| `cluster.domain` | `--cluster-domain` | |
| `kubeletClientCA.source`, `inline`, `configMap.*`, `secret.*` | `--kubelet-client-ca-file` | `none`, `inline`, an existing `configMap` or `secret`, or `extensionApiserverAuthentication` (copies key `client-ca-file` of `kube-system/extension-apiserver-authentication` into a ConfigMap of the release with Helm's `lookup`; the API server's client CA: the k3s client CA, the cluster CA on kubeadm and EKS). kubeadm and EKS need none; on k3s without it `kubectl logs` and `exec` fail with 401. |
| `vendors.<vendor>.enabled` (`alicloud`, `aws`, `azure`, `gcp`, `hcloud`, `oci`, `ovh`, `upcloud`, `vultr`), `vendors.extra` | `--vendors` | `alicloud`, `aws`, `azure`, `gcp`, `hcloud`, `oci`, `ovh`, `upcloud` and `vultr` if enabled, then the extra drivers (the development image's `fake` and `qemu`); at least one is required. |
| `vendors.alicloud.enabled`, `homeRegion`, `callTimeout` | `--vendors`, `SCARPENTER_ALICLOUD_HOME_REGION`, `SCARPENTER_ALICLOUD_CALL_TIMEOUT` | |
| `vendors.azure.enabled`, `vendors.azure.subscriptionID` | `--vendors` (adds `azure`), `AZURE_SUBSCRIPTION_ID` | `subscriptionID` is required when enabled. |
| `vendors.azure.ipv4Price` | `SCARPENTER_AZURE_IPV4_PRICE` | An explicit override that wins over Keeper's price, see the table above. |
| `vendors.azure.credentials.mode`, `workloadIdentity.*`, `secret.*` | `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`, a projected ServiceAccount token, `AZURE_CLIENT_SECRET` | [Azure credentials](../how-to-guides/set-up-azure.md#2-credentials). `workloadIdentity` needs `clientId` and `tenantId`, `secret` an `existingSecret`. |
| `vendors.gcp.enabled` | `--vendors` | `gcp` is added to the list when enabled (off by default). |
| `vendors.gcp.project` | `--gcp-project` | |
| `vendors.gcp.preemptionNotice` | `--gcp-preemption-notice` | Empty or `0s`: no notice. |
| `vendors.gcp.ipv4Price` | `SCARPENTER_GCP_IPV4_PRICE` | An explicit override that wins over Keeper's price, see the table above. |
| `vendors.gcp.credentials.mode`, `workloadIdentity.*`, `secret.*` | `GOOGLE_APPLICATION_CREDENTIALS`, a projected ServiceAccount token (audience `workloadIdentity.audience`) and a ConfigMap or Secret mount | [GCP credentials](../how-to-guides/set-up-gcp.md#2-credentials). `workloadIdentity` needs `configMap` and `audience`, `secret` an `existingSecret`. |
| `vendors.oci.enabled`, `vendors.oci.compartment` | `--vendors` (adds `oci`), `--oci-compartment` | `compartment` is required when enabled. |
| `vendors.oci.imageCompartment` | `--oci-image-compartment` | Optional; empty (the default) means `compartment`. |
| `vendors.oci.interruptionPoll` | `--oci-interruption-poll` | Optional; empty (the default) means `10s`. |
| `vendors.oci.credentials.mode`, `apiKey.*`, `workloadIdentityFederation.*`, `configFile`, `configProfile` | `--oci-auth`, `--oci-credentials-dir`, `--oci-region`, `--oci-token-file`, `--oci-identity-domain-url`, `--oci-client-id`, `--oci-client-secret-file`, `--oci-config-file`, `--oci-config-profile`, a Secret mount and a projected ServiceAccount token (audience `workloadIdentityFederation.audience`) | [OCI credentials](../how-to-guides/set-up-oci.md#2-credentials). `apiKey` needs an `existingSecret`; `workloadIdentityFederation` needs `region`, `identityDomainURL`, `clientId`, `audience` and `clientSecret.existingSecret`; `none` wires nothing, not even `--oci-auth` (`configFile` and `configProfile` apply there). |
| `vendors.aws.homeRegion` | `--aws-home-region` | |
| `vendors.aws.region` | `AWS_REGION` | Default: the home region, else `us-east-1`. |
| `vendors.aws.ipv4Price` | `SCARPENTER_AWS_IPV4_PRICE` | An explicit override that wins over Keeper's price, see the table above. |
| `vendors.ovh.enabled` | `--vendors` | Adds `ovh` to the list. |
| `vendors.ovh.scheduleWait`, `regionAliases` | `SCARPENTER_OVH_SCHEDULE_WAIT`, `SCARPENTER_OVH_REGION_ALIASES` | A map of aliases is rendered as `KEEPER=API` pairs, sorted. |
| `vendors.ovh.credentials.mode`, `secret.*` | `OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`, `OS_PROJECT_ID`, `OS_USER_DOMAIN_NAME` | [OVH credentials](../how-to-guides/set-up-ovh.md#2-credentials). `secret` needs an `existingSecret`; the `*Key` values name its keys; `none` wires nothing. |
| `vendors.alicloud.credentials.mode`, `oidc.*`, `secret.*` | `ALIBABA_CLOUD_ROLE_ARN`, `ALIBABA_CLOUD_OIDC_PROVIDER_ARN`, `ALIBABA_CLOUD_OIDC_TOKEN_FILE`, a projected ServiceAccount token, `ALIBABA_CLOUD_ACCESS_KEY_ID`, ... | [Alibaba Cloud credentials](../how-to-guides/set-up-alicloud.md#2-credentials). `oidc` needs `roleArn` and `providerArn`, `secret` an `existingSecret`. |
| `vendors.upcloud.credentials.mode`, `secret.existingSecret`, `secret.tokenKey` | `UPCLOUD_TOKEN` | `secret` (the default) needs an `existingSecret`; `none` wires nothing (supply the variable through `extraEnv`). |
| `vendors.upcloud.networkRange` | `SCARPENTER_UPCLOUD_NETWORK_RANGE` | An IPv4 /16; empty: `10.241.0.0/16`. |
| `vendors.aws.credentials.mode`, `webIdentity.*`, `secret.*` | `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, a projected ServiceAccount token, `AWS_ACCESS_KEY_ID`, ... | [AWS credentials](../how-to-guides/set-up-aws.md#2-credentials). `webIdentity` needs `roleArn`, `secret` an `existingSecret`. |
| `vendors.hcloud.credentials.secret.existingSecret`, `key` | `HCLOUD_TOKEN` | The Secret that holds the project's API token ([Hetzner Cloud driver](hcloud-driver.md)); the name is required when the driver is enabled. |
| `vendors.hcloud.serverLimit` | `--hcloud-server-limit` | `0`: unknown (flag left out). |
| `vendors.hcloud.buildImages` | `--hcloud-build-images` | `true`: the driver builds missing snapshots (flag left out); `false` passes `--hcloud-build-images=false`. |
| `vendors.hcloud.ipv4Price` | `SCARPENTER_HCLOUD_IPV4_PRICE` | An explicit override that wins over Keeper's price, see the table above. |
| `vendors.vultr.enabled` | `--vendors` | Adds `vultr` to the list. Requires `credentials.secret.existingSecret`. |
| `vendors.vultr.credentials.secret.existingSecret`, `key` | `VULTR_API_KEY` | The API key from the Secret (key default `apiKey`); the only credential mode. [Vultr credentials](../how-to-guides/set-up-vultr.md#2-credentials). |
| `vendors.vultr.availabilityTTL` | `SCARPENTER_VULTR_AVAILABILITY_TTL` | |
| `keeper.url` | `--keeper-url` | The public Keeper needs an API key. |
| `keeper.apiKey.existingSecret`, `key` | `--keeper-api-key-file` | The Secret's key (default `api-key`) is mounted as a file and read at every refresh; [Keeper API key](../how-to-guides/install.md#keeper-api-key). |
| `keeper.cacheDir.sizeLimit` | `--keeper-cache-dir` | Size limit (default `256Mi`) of the `emptyDir` at `/var/cache/scarpenter/keeper`; the flag is always set. |
| `catalog.refreshInterval`, `maxAge` | `--catalog-refresh-interval`, `--catalog-max-age` | |
| `artifacts.sysextBaseURL`, `flatcarBaseURL` | `--sysext-base-url`, `--flatcar-base-url` | |
| `nvidia.driverBaseURL` | `--nvidia-driver-base-url` | The mirror of the NVIDIA driver sysexts ([NVIDIA GPU nodes](node-bootstrap.md#nvidia-gpu-nodes)). |
| `bootstrap.tokenTTL` | `--bootstrap-token-ttl` | At least 20m; the chart refuses less. |
| `bootstrap.tailnetPrefixes` | `--tailnet-prefixes` | A list of CIDRs. |
| `tailnet.backend`, `apiURL`, `keyTTL` | `--tailnet-backend`, `--tailnet-api-url`, `--tailnet-key-ttl` | `backend` is `none` (default), `tailscale` or `headscale`; `apiURL` is required with `headscale`. The flags other than the backend are passed only when set. |
| `tailnet.credentials.existingSecret`, `clientIdKey`, `clientSecretKey`, `apiKeyKey` | `--tailnet-credentials-dir` | Required with a backend (the schema refuses a backend without it); not mounted with `none`. The Secret of the release namespace is mounted read-only into the controller at `/etc/scarpenter/tailnet`: `clientIdKey` and `clientSecretKey` (defaults `client-id`, `client-secret`) as `client-id` and `client-secret` with `tailscale`, `apiKeyKey` (default `api-key`) as `api-key` with `headscale`. The Pod spec mounts it, so the controller needs no RBAC on Secrets for it. |
| `launch.maxAttempts` | `--max-launch-attempts` | The default of every class; `spec.launch.maxAttempts` of an SCNodeClass overrides it ([launch policy](#launch-policy)). |
| `quota.refreshInterval`, `limitsRefreshInterval`, `maxStaleness`, `launchableRefreshInterval` | `--quota-*` | |
| `gc.interval`, `gracePeriod`, `terminatingGrace`, `dryRun` | `--gc-interval`, `--gc-grace-period`, `--terminating-grace`, `--gc-dry-run` | |
| `billingRetention.enabled` | `--billing-retention` | `true`; the chart passes the flag only when it is `false`. |
| `interruption.enabled`, `margin`, `unavailableTTL` | `--interruption`, `--interruption-margin`, `--interruption-unavailable-ttl` | `enabled` is `true`; the chart passes `--interruption` only when it is `false`. |
| `unavailable.insufficientCapacityTTL`, `unsupportedTTL`, `quotaExceededTTL`, `zoneUnusableTTL` | `--unavailable-*-ttl` | `spec.launch.insufficientCapacityTTL` of an SCNodeClass can extend the first for that class ([launch policy](#launch-policy)). |
| `managedResources.defaultTTL`, `perUseGrace`, `checkInterval`, `fullSweepEvery` | `--resource-default-ttl`, `--resource-per-use-grace`, `--resource-check-interval`, `--resource-full-sweep-every` | See the cost note of the full sweep. |
| `imageCopies.idleTTL` | `--image-copy-idle-ttl` | Empty keeps `720h`. |
| `vendors.aws.imageCopies.enabled`, `vendors.alicloud.imageCopies.enabled` | `--aws-copy-images`, `--alicloud-copy-images` | `false`; the chart passes the flag only when the vendor is enabled and the value is `true` ([Image copies](image-copies.md)). |
| `karpenter.logLevel`, `batchMaxDuration`, `batchIdleDuration`, `registrationTimeout`, `featureGates`, `metricsPort`, `healthProbePort` | `--log-level`, `--batch-max-duration`, `--batch-idle-duration`, `--registration-timeout`, `--feature-gates`, `--metrics-port`, `--health-probe-port` | Core's. |
| `leaderElection.enabled` | `DISABLE_LEADER_ELECTION` | With more than one replica leave it on. |
| `hostNetwork` | | Runs the pod in the node's network namespace (see [Host network](../how-to-guides/install.md#host-network)); `metricsPort` and `healthProbePort` are then host ports. |
| `replicaCount`, `podDisruptionBudget.*` | | The PodDisruptionBudget is rendered with more than one replica. |
| `image.repository`, `tag`, `digest`, `pullPolicy`, `imagePullSecrets` | | `ghcr.io/sparecores/scarpenter` (production drivers only), tag = the chart's `appVersion`. The development image is `ghcr.io/sparecores/scarpenter-dev`. The chart needs Kubernetes 1.31 or newer. |
| `serviceAccount.*`, `rbac.*` | | `rbac.authKeySecrets`: a Role (`get` on that one Secret) and a RoleBinding for tailscale auth key Secrets in other namespaces; `rbac.extraRules` are appended to the controller's ClusterRole. |
| `metrics.offeringDetail` | `--metrics-offering-detail` | `false`; the chart passes the flag only when it is `true`. |
| `metrics.*`, `serviceMonitor.*` | | A Service for the metrics port and an optional ServiceMonitor (needs the Prometheus Operator CRD). |
| `examples.*` | | An example `SCNodeClass` and `NodePool` per enabled vendor. |
| `resources`, `affinity`, `nodeSelector`, `tolerations`, `topologySpreadConstraints`, `priorityClassName`, `podSecurityContext`, `securityContext`, probes, `podAnnotations`, `podLabels`, `extraArgs`, `extraEnv`, `extraVolumes`, `extraVolumeMounts` | | The default affinity prefers nodes without the `karpenter.sh/nodepool` label (the controller must not run on a node it manages). The default resources come from the L3 scale measurement: about 310 MiB peak with three AWS regions and 430 MiB with every AWS region; the chart limit is 2Gi (see [Memory sizing](#memory-sizing)). |

## Permissions

The chart's RBAC is generated from `config/rbac` (SCarpenter) and
`config/rbac-core` (Karpenter core) by `make chart-rbac`; `make test-chart` renders the
chart and fails when its rules or bindings differ from those files. Kubernetes
objects, all bound to the controller's ServiceAccount:

| Object | Grants | For |
|---|---|---|
| ClusterRole `scarpenter` | `scnodeclasses` get, list, watch, update, patch and `scnodeclasses/status` get, update, patch; `nodeclaims` get, list, watch, delete; `nodepools`, `pods`, `certificatesigningrequests` get, list, watch; `nodes` get, list, watch, patch; `update` on `certificatesigningrequests/approval`; `approve` on the signer `kubernetes.io/kubelet-serving` only; `get` on ConfigMaps (references name their namespace) | the NodeClass controllers (server-side apply of the status), the serving certificate approver (it reads the NodeClaims, the Nodes and, through the driver registry, the SCNodeClasses), ConfigMap references and the cluster CA, and the [billing retention controller](#billing-retention) and the [interruption controller](#interruption): they patch the Nodes (the hold annotations, the `billing-start` annotation of a node launched before it existed, the `releasing` taint, the interruption annotations), deletes the NodeClaim of an empty node at the end of its paid period, and reads the pods of a node and the NodePools (disruption budgets, limits) |
| ClusterRole `scarpenter-core` | Karpenter core's: NodePools, NodeClaims and NodeOverlays (full), CapacityBuffers, pods, volumes, namespaces, services, events, storage classes, CSI nodes, volume attachments, workloads, PodDisruptionBudgets, CRDs (read), `pods/eviction` create, `pods` delete, Nodes get, list, watch, patch, update, delete, Events create and patch, leases get, list, watch | scheduling and disruption |
| Role `scarpenter-bootstrap-tokens` in `kube-system` | Secrets get, list, create, delete | the bootstrap tokens (RBAC cannot limit `get` and `list` by label: the controller can read every Secret of `kube-system`) |
| Role `scarpenter-cluster-dns` in `kube-system` | Services get, list | the cluster DNS Service, read when `--cluster-dns` is not set |
| Role `scarpenter-offered-types` in the release namespace | ConfigMaps list, create, patch, delete | the [offered types listing](labels-and-offerings.md#the-offered-listing): applied server-side for the SCNodeClasses that ask for it, found at start by the label `app.kubernetes.io/managed-by=scarpenter`, removed with the annotation |
| Role `scarpenter-auth-key` in the release namespace | Secrets get | the tailscale auth key Secret (`rbac.authKeySecrets` for other namespaces) |
| Role `scarpenter-leader-election` in the release namespace | leases (full) | core's leader election |
| ClusterRoleBindings `scarpenter-node-bootstrapper`, `scarpenter-auto-approve-node-client-csrs`, `scarpenter-auto-approve-node-client-renewals` | Kubernetes' `system:node-bootstrapper` and the `nodeclient` and `selfnodeclient` CSR approver roles for the group `system:bootstrappers:scarpenter` and `system:nodes` | the node join ([node bootstrap](node-bootstrap.md#rbac)) |

**No ClusterRole grants access to Secrets.** SCarpenter creates no Nodes (the
kubelets register them), and no state store is needed: its state is the cluster
objects, the vendor's resources and their tags.

The controller's network access is outbound only: the Kubernetes API, the Keeper
API (HTTPS: `/healthcheck`, the snapshot routes `/snapshot/<hash>/...` and `/benchmark_configs`; no `/storage_prices`: the vendors' storage and IPv4 prices are files of the snapshot), the
vendors' APIs and the artifact hosts (or their mirrors). It listens on the metrics
and health ports only; no webhook, no admission controller.

## SCNodeClass status conditions

| Condition | Meaning | Set by |
|---|---|---|
| `Ready` | Root condition: true when `CatalogReady`, `ArtifactsReady` and `ValidationSucceeded` are. A vendor that is not ready only makes its own offerings unavailable. | derived (operatorpkg) |
| `CatalogReady` | The catalog snapshot for the SCNodeClass is available and not older than `--catalog-max-age`. The catalog computes it (`Catalog.Status`: `NoSnapshot`, `KeeperAPIKeyRequired` (no catalog because Keeper answered 401), `ScopePending`, `Stale`); the NodeClass controller writes it to the status. When `True`, the message names the Keeper data version (hash) and when it was published. | plan 03 (status), plan 05 (condition), plan 13 |
| `KeeperAuthorized` | Informational, not a dependent of `Ready` (like `RestartRequired`): `False` with reason `APIKeyMissing` (no key configured) or `APIKeyRejected` (Keeper rejected the one sent) after Keeper answered a refresh with 401, the message naming `--keeper-api-key-file` / `keeper.apiKey.existingSecret` and the [Keeper API key](../how-to-guides/install.md#keeper-api-key) section; `True` (`Authorized`) after the next successful snapshot read; absent before either. The catalog computes it (`Catalog.Status`); the NodeClass controller writes it. | plan 13 |
| `ArtifactsReady` | The sysext images for the pinned versions are resolved (`status.resolved`). Reasons: `Resolved`, `ReleaseNotFound`, `ResolutionFailed`. | plan 05 |
| `ValidationSucceeded` | The semantic checks pass: kubelet quantities parse, the Kubernetes version is within the skew of the API server (at most three minors older, never newer), the referenced ConfigMaps and Secret exist, a trial rendering of the user-data succeeds within the size budget, and the cluster DNS is known (`--cluster-dns`, or found). Reasons: `Valid`, `InvalidKubeletSettings`, `KubernetesVersionNotSupported`, `ReferenceNotFound`, `ReferenceUnreadable`, `TemplateInvalid`, `ClusterDNSUnknown`, `InvalidResourceUse`, `TailnetKeysNotConfigured`. `TailnetKeysNotConfigured`: a `PerNode` NodeClass while the controller has no tailnet backend (`--tailnet-backend none`) or no credentials directory, or whose `loginServer` does not match the backend; the check calls no API. A `use` block of a vendor resource that selects nothing (an empty `use`, which an old object stored before a field was renamed has after the API server pruned the field) is `InvalidResourceUse`, never "nothing given": the controller does not fall back to the vendor's default or to creating the resource. | plan 05, plan 14 |
| `BillingRetention` | Informational, not a dependent of `Ready`: `True` (`Active`) when billing retention is active for at least one vendor of the NodeClass, else `False` (`Disabled` or `NotActive`). The message gives the reason per vendor and counts the nodes left alone per reason (`ConsolidationNever`, `UserDoNotDisrupt`, `StaticPool`, `Spot`, `Drifted`). See [Billing retention](#billing-retention). | plan 23 |
| `RestartRequired` | Informational, not a dependent of `Ready`: a metric was added or renamed after start-up; its labels are selectable only after the controller restarts (see [labels](labels.md)). The catalog names the keys (`Catalog.RestartRequired`) and stops stamping them; the NodeClass controller writes the condition. | plan 03 (keys), plan 05 (condition) |

Per vendor, `status.vendors.<vendor>.conditions`:


| Condition | Meaning | Set by |
|---|---|---|
| `VendorReady` | The vendor is enabled, at least one region the NodeClass names can be used and the driver's `ValidateNodeClass` accepts the block. One region the credentials cannot use does not stop the vendor (see `RegionsUnavailable`). Reasons: `Ready`, `VendorNotEnabled`, `UnknownRegions` (a region name that neither the driver nor Keeper has for the vendor, a typo), `NoUsableRegions` (every named region is unavailable), `ValidationFailed`. | plan 05 |
| `ImagesReady` | Boot images exist per region and architecture, as the driver's optional `ImageReporter` reports them (`status.vendors.<vendor>.images`); a driver without it resolves images at launch and counts as ready. `False` only when a region the NodeClass names has no image or when no region has one; otherwise `True` with reason `ImagesPartial` naming the regions without. With image copies on, an image that is queued or in flight for a copy does not count as missing: the condition is `True` with reason `ImagesCopying` (also for a region the NodeClass names), and a region that only waits for a copy on demand (`ImageCopyOnDemand`) is `True` with `ImagesPartial`; a failed copy is a missing image ([Image copies](image-copies.md#image-status)). An image that is not the pinned Flatcar release (the `images` selector or the convention found an image that says another version, or none) makes it `False` with the image's own reason, `ImageVersionMismatch` or `ImageVersionUnknown`, naming the region and the image, whether or not the NodeClass names the region: the image is what the NodeClass chose, and it is never booted ([ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)). When the images are not ready for another cause, an image that blocks (in a region the NodeClass names, or any when no region has one) may carry the driver's own reason, which then becomes the condition's reason with the image's message (Azure: `MarketplaceTermsNotAccepted`; hcloud: `ImageBuilding` while the driver builds the snapshot, `ImageBuildFailed` after a failed build, each with an Event of the same reason, [ADR-030](hcloud-driver.md#building-the-snapshot)); otherwise the reason is `ImagesNotReady`. The version reasons take precedence. The offerings of a region in which no image is ready are unavailable; the NodeClass stays `Ready`. | plan 05 (hook), plan 08 (AWS), plan 14 (A1) |
| `ResourcesReady` | `False` (`ResourcesUnavailable`) when a region lacks a resource the NodeClass needs (a `use` selector matching nothing, or more than its `select` allows (the message carries the candidates after each stage, see [resource selectors](resource-selectors.md#resolution-status-and-caching)), a `forbid` kind without a vendor default, a preCreate resource that cannot be created): the offerings of that region are unavailable, the message names region and kind; `Unknown` (`CheckFailed`) when the vendor could not be asked. Informational for `Ready`. See [vendor resources](vendor-resources.md#status). | plan 06 |
| `QuotaAvailable` | Informational: `False` when a quota bucket has no headroom. | plan 04 |
| `RegionsUnavailable` | Informational and abnormal-true, like `ConfigurationWarning`: present and `True` (`RegionsUnavailable`) while some regions the NodeClass names cannot be used, and the vendor is ready through its other regions. The message lists them. A region is unavailable when Keeper has it but the driver does not list it (an AWS opt-in region that is not enabled, an Azure location outside the subscription), when the credentials cannot read its zones (the driver's `RegionUnavailable`, for example an Alibaba Cloud `cn-*` region), or when its zones failed to load and are retried with backoff (10 s up to 15 min; the message says "retrying", and such a region does not count toward `NoUsableRegions`). A region named by a Keeper alias or ID counts as the driver's region of that name. A region has no zones, so no offerings, and no images are asked for it: the other regions and `ImagesReady` are unaffected. The region verdict waits ("pending") until the driver's and Keeper's region lists are loaded, so a vendor whose region list never loads keeps its previous verdict. While they are not loaded (after a restart) there is no verdict: the previous `RegionsUnavailable` and region-based `VendorReady` stay as they were. Absent when all regions work, never `False`; it clears on the next reconcile after the region can be read. A Warning Event `RegionsUnavailable` and a log line are published when the set changes. | |
| `ConfigurationWarning` | Informational and abnormal-true (like `RestartRequired`): present and `True` (`SettingHasNoEffect`) only while the driver's optional `NodeClassWarner` reports a setting that is valid but has no effect for the vendor (Azure: `networking.isolation` next to a brought network security group); absent otherwise, never `False`. Nothing depends on it: the vendor stays `VendorReady`. A Warning Event `ConfigurationWarning` is published when the warning appears or its text changes, not on every reconcile. | plan 17 |

The NodeClass controller (plan 05; see [node bootstrap](node-bootstrap.md#the-nodeclass-controller))
writes these conditions, the resolved artifacts and the per-vendor drift hash with
server-side apply under the field manager `scarpenter-nodeclass`; the quota status
controller sets `QuotaAvailable` (see [Quota](#quota)) under `scarpenter-quota`, the
resource status controller `ResourcesReady` and `resources` (see
[vendor resources](vendor-resources.md#status)) under `scarpenter-resources`, and the
offered types controller `offered` (see
[the offered summary](labels-and-offerings.md#the-offered-summary-in-the-status))
under `scarpenter-offered`.
`VendorReady` also requires that the vendor block and every profile merge with the
driver's defaults (`ValidationFailed` otherwise).
Development builds run the same controller; the stand-in of plan 04 is gone.

`Create` needs `Ready=True` with the current `observedGeneration`. A vendor that
is not ready offers no instance types; a `Create` fails with `NodeClassNotReady`
only when every vendor the NodeClaim allows is not ready (when another, ready
vendor simply has no capacity, it is `InsufficientCapacity`).

### Memory sizing

The memory of the controller is the catalog it holds: the data of one Keeper
data version in scope and the instance types built from it (one offering per
price row of a zone). Measured on the committed recording (Keeper database
`2c401c216`) with three benchmark metrics; "refresh peak" is the resident set
while the files are read, "built" the Go heap after a GC with every type of the
scope built, and the controller rows are the L3 scale scenario S10, at rest:

| Scope | Offerings | Refresh peak (RSS) | Data held | Built (heap) |
|---|---|---|---|---|
| AWS, three regions | 26,303 | 77 MiB | 18 MiB | 87 MiB (controller 239 MiB RSS at rest, peak 310) |
| AWS, every region | 134,992 | 116 MiB | 42 MiB | 303 MiB (controller 384 MiB at rest, peak 430) |
| Azure, every region | 294,674 | 197 MiB | 68 MiB | 571 MiB |
| All eight vendors, every region | 520,386 | 269 MiB | 131 MiB | 1,078 MiB |

The chart's default limit is 2Gi (request 640Mi), which covers every region of
all eight vendors; scope the NodeClasses' `regions` to what you use to need less.
The refresh itself reads each file as a stream and holds no uncompressed body.

## Initialization timeout

A NodeClaim is Initialized when its node is Ready, its startup taints are gone and
its resources are registered. Core's liveness check only covers the time until
registration, and consolidation, emptiness and drift skip uninitialized nodes, so
a node that registered but never initializes (for example because the kubelet
cannot pull the pause image) would run and be billed until it is deleted by hand,
or by `expireAfter`.

The initialization timeout controller watches the NodeClaims of SCNodeClasses.
For one that is Registered but not Initialized and not being deleted it measures
the time since the creation of its Node (the Registered condition's time when
there is no Node). Once that exceeds `spec.launch.initializationTimeout` of the
class (default `1h`; `0s` disables the check) it deletes the NodeClaim, and core
terminates the instance. Until then it requeues at the deadline (at most 10
minutes ahead, also with the check disabled) instead of polling. A changed timeout applies within about 10 minutes.

The deletion is announced by a Warning Event `InitializationTimeout` on the
NodeClaim and on the NodeClass, naming the elapsed time and the Ready condition's
reason and message of the node, by a log line, and by the counter
`scarpenter_nodeclaims_initialization_timeout_total{vendor,nodeclass}`.

The [GPU pending controller](#gpu-pending-taint) deletes a GPU node whose driver
check has not passed after 30 minutes. That is inside the default timeout, so
the two agree; a class that sets a shorter timeout makes this controller act
first. Both only delete the NodeClaim.

## GPU pending taint

The GPU pending controller removes the taint `scarpenter.sparecores.com/gpu-pending` from a Node that has the label
`scarpenter.sparecores.com/gpu-ready=true` ([ADR-031](../explanation/DECISIONS.md#adr-031-gpu-nodes-register-before-the-check-and-fabric-manager-work-are-done-accepted-2026-11-22)).
The node's own check sets the label with the kubelet's credentials, because NodeRestriction forbids a node to change its taints.
It also removes the taint at once from every Node of a SCarpenter NodeClaim without the `nvidia-driver-branch` label (a CPU
node of a pool that lists the taint in `startupTaints`, as every pool that can launch GPU types must). A GPU node labelled `gpu-ready=false`, or still tainted 30 minutes after its creation, loses its NodeClaim (Event `GPUCheckFailed`).
The controller already has `patch` on Nodes and `delete` on NodeClaims; other taints are left alone.

## Billing retention

The billing retention controller ([ADR-024](../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10),
plan 23; how to use it: [Reuse nodes within the billing period](../how-to-guides/reuse-nodes-within-billing-period.md))
holds the nodes of a NodeClass with `spec.billing.retainUntilPeriodEnd: true` until the end of the billing period
they have paid for and deletes the empty ones before the next charge starts. It is
the package `pkg/controllers/retention`, started with `--billing-retention` (default on) and built with
Karpenter's own cluster state, so nomination, mark-for-deletion and the disruption-budget accounting are
Karpenter's.

**The rule.** Keeper's vendor table gives a vendor's billing increment and minimum
(`billing_increment_seconds`, `minimum_billing_seconds`, `stopped_server_charged`), read with the catalog refresh
(a failed or missing table never fails it: the status says `NoKeeperData`). The driver's optional
`BillingDescriber` overrides it field by field per region and family and says whether the rule was verified on a live
account ([driver contract](driver-contract.md#billing)); a driver without it is unverified and retention is off for it.
Two shapes are supported: increments held (increment `>= minPeriod`) with a minimum equal to the increment or none (hourly), and
minimum only (increment `< minPeriod`, such as a minimum then per second: one boundary, `start + minimum`). A minimum different
from a held increment is `UnsupportedShape`. The boundaries count from
the instance's `BillingStart` (annotation `scarpenter.sparecores.com/billing-start`, RFC 3339), never from the NodeClaim's
time; for a NodeClaim that has none the controller asks the cloud provider once and writes it to the Node. The effective margin is the larger of
`spec.billing.margin` and the rule's `DeleteLatency` plus 90 s.

**What it does to a node**, per Node owned by a NodeClaim of an opted-in NodeClass, on-demand, registered, of a
non-static NodePool whose `consolidateAfter` is not `Never`, not drifted, without a `karpenter.sh/do-not-disrupt` of its own:

1. Before the deadline (`boundary - margin`): `karpenter.sh/do-not-disrupt: "true"` and
   `scarpenter.sparecores.com/retain-until: <deadline>` on the Node, from registration on (Karpenter never disrupts a node that is not
   Initialized, so no window is left). Only the annotations carrying this marker are ever removed.
2. At the deadline an **empty** (no pod except DaemonSet, static and finished ones) and Initialized node is released: not nominated
   (else look again in 20 s), the NodePool's `Empty` budget allows a deletion (else the node stays held into the next period and
   `ReleaseBlockedByBudget` is published), `MarkForDeletion` in cluster state, the `NoSchedule` taint
   `scarpenter.sparecores.com/releasing` (its value says why; its `timeAdded` carries the 5 s wait that Karpenter's `MinDrainTime` also
   uses), then the pods are read again from the API server and the nomination checked again: a hit undoes the mark and the taint and the node is busy,
   otherwise the NodeClaim is deleted. A busy node loses its hold for the window to the boundary and is held again for the next period; at
   the last boundary of a minimum-only rule the hold is gone for good.
3. **Pressure.** A pending pod that Karpenter has not mapped to a NodeClaim and that a NodePool's `spec.limits` leave no room for, a vendor
   quota refused by the cloud provider (`OnQuotaRefusal`) or blocked in the quota tracker for every offering of a NodeClass, or more held empty
   nodes than `spec.billing.maxRetainedNodes`, releases held empty nodes (nearest deadline first, ties by oldest billing start), as many as cover the request,
   with the same budget check and sequence. A candidate that a budget blocks loses its hold until the end of its period (annotation
   `scarpenter.sparecores.com/hold-released-until`, so neither a reconcile nor a restart puts it back) and the next is tried. The signals are read every 10 s.

A restart changes nothing: the state is on the Node (annotations and taint), the NodeClaim and the clock. The budget check and the mark are
serialised; a race with Karpenter's own disruption (its 15 s re-validation) can exceed a budget by one node, which
`scarpenter_billing_budget_overshoot_total` counts.

**Annotations and taint** (see [labels](labels.md)): `scarpenter.sparecores.com/billing-start` (NodeClaim, Node),
`scarpenter.sparecores.com/retain-until` and `scarpenter.sparecores.com/hold-released-until` (Node), taint `scarpenter.sparecores.com/releasing:NoSchedule` (Node).

**Status**: `status.billing` lists per vendor of the NodeClass `{vendor, active, reason, message, period, source}` and the condition
`BillingRetention` summarises it, under the field manager `scarpenter-billing`. Reasons: `Active` (the message says `increments` or
`minimum only`), `Disabled`, `PerSecondBilling` (no minimum), `MinimumTooShort` (minimum less the margin under the 2 minute hold
floor), `Unverified`, `UnsupportedShape`, `NoKeeperData`, `NoBillingStart` (every node of the vendor lacks a stamp), `PeriodTooShort`
(a held period under twice the effective margin) and `InvalidSettings` (`minPeriod` under twice the margin). The
status shows the vendor-wide rule (a driver is asked with an empty region and family); a node's own rule is the one of its region and family.
At start-up, and when it changes, the controller logs the resolved rule per vendor (increment, minimum, period, source, verified).

**Events** on the Node: `RetainedUntil` (Normal), `ReleasedAtPeriodEnd` (Normal), `ReleasedUnderPressure` (Normal),
`ReleaseBlockedByBudget` (Warning), `RetentionSkipped` (Normal, once per node and reason: `Spot`, `StaticPool`,
`ConsolidationNever`, `Drifted`, `UserDoNotDisrupt`) and `BillingStartUnknown` (Warning, once).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_billing_retained_nodes` | gauge | `vendor`, `nodeclass` | Nodes held until the end of their period. |
| `scarpenter_billing_retained_seconds_total` | counter | `vendor` | Node seconds spent held (counted at the status interval, 15 s). |
| `scarpenter_billing_reuse_total` | counter | `vendor` | A node that was empty under the hold received a pod. |
| `scarpenter_billing_periods_saved_estimate_total` | counter | `vendor` | Equals the reuses: an estimate, not an invoice. |
| `scarpenter_billing_deletes_at_deadline_total` | counter | `vendor` | Empty nodes deleted before the end of their period. |
| `scarpenter_billing_blocked_by_budget_total` | counter | `vendor` | Releases that a disruption budget blocked. |
| `scarpenter_billing_released_under_pressure_total` | counter | `vendor`, `scope` | Releases for a launch blocked by `limit` or `quota`, or by `max-retained`. |
| `scarpenter_billing_skipped_nodes` | gauge | `vendor`, `nodeclass`, `reason` | Nodes left alone (`ConsolidationNever`, `UserDoNotDisrupt`, `StaticPool`, `Spot`, `Drifted`). |
| `scarpenter_billing_boundary_missed_total` | counter | `vendor` | An empty node held again 30 s past its boundary without a budget block: the margin is too small. |
| `scarpenter_billing_budget_overshoot_total` | counter | `vendor` | The recount after a mark found the budget exceeded (the race with Karpenter). |
| `scarpenter_billing_boot_seconds` | histogram | `vendor` | Seconds from the billing start to Initialized: whether a short minimum leaves a window. |

## Interruption

The interruption controller ([ADR-027](../explanation/DECISIONS.md#adr-027-spot-interruption-the-notice-is-an-annotation-the-reaction-is-one-controller-accepted-2026-11-19),
plan 26) reacts when a vendor announces that it will reclaim a spot or preemptible node, so that Karpenter drains the node
as early in the notice window as possible and every Pod gets its SIGTERM with the time the vendor allows. It is the package
`pkg/controllers/interruption`, started with `--interruption` (default on). How a workload sees an interruption and counts
them is in the how-to guide [Handle spot interruptions](../how-to-guides/handle-spot-interruptions.md).

**The notice** is three annotations on the Node (see [labels](labels.md#interruption-annotations)):
`scarpenter.sparecores.com/interruption` (the reason, `SpotInterrupted`),
`scarpenter.sparecores.com/interruption-deadline` (RFC 3339) and `scarpenter.sparecores.com/interruption-source`
(`instance-metadata` for the node agent, `vendor-api` for the controller's own poller; anything else is read as
`instance-metadata`). The poller is described [below](#the-poller); the node agent comes with the vendor plans. The controller reads
only the Node.

**What it does**, per annotated Node whose NodeClaim belongs to an SCNodeClass and whose provider ID and node name match the claim's
(a node cannot name another node's claim), in this order:

1. A NodeClaim that is not spot (`karpenter.sh/capacity-type`) or a reason it does not know is ignored, with one `InterruptionIgnored` Warning Event
   on the Node and the NodeClaim.
2. The deadline is clamped into `[now, now + the driver's longest notice]`; a missing or unparsable one is `now + the driver's shortest notice`
   ([notice window](driver-contract.md#interruption-notice-window); a driver that declares none gets 30 s and 5 min). The effective deadline is written
   back to the Node and the NodeClaim, and a later reconcile keeps the NodeClaim's once it has the `Interrupted` condition (before that, a deadline
   on the NodeClaim, for example from a NodePool template, is not trusted).
3. The NodeClaim's `karpenter.sh/nodeclaim-termination-timestamp` is tightened to `deadline - --interruption-margin`, or to the deletion time plus the
   NodeClaim's `terminationGracePeriod` when that is earlier (core sets the latter only on a claim without a timestamp); never extended: an earlier one is kept,
   and the three annotations are copied to the NodeClaim. Karpenter's termination controller then force-deletes, with the grace clamped to the time
   left (at least 1 s), every Pod whose grace would outlast the deadline, PDBs and `do-not-disrupt` included, and stops waiting for the drain at the
   deadline. This also applies to a NodeClaim that is already being deleted (for example by consolidation).
4. The NodeClaim gets the condition `Interrupted` and the condition `DisruptionReason`, both `True` with the reason `SpotInterrupted`. `Interrupted` is
   SCarpenter's own (core clears `DisruptionReason` on nodes it does not consider disrupting; once the NodeClaim is deleting, it does not);
   `DisruptionReason` makes core's eviction Events and `karpenter_pods_drained_total` carry the reason.
5. The offering is marked unavailable in the unavailable-offerings cache: (vendor, instance type, zone, `spot`) for `--interruption-unavailable-ttl`;
   on-demand offerings of the same type and zone stay. Metrics and the `SpotInterrupted` Warning Events (Node and NodeClaim) follow.
6. The NodeClaim is deleted; Karpenter core taints the node `karpenter.sh/disrupted:NoSchedule`, drains it and deletes the instance.

Every step is idempotent. What must happen once (the cache entry, the metrics, the Events) happens when the NodeClaim has no `Interrupted`
condition yet, so a second reconcile or a restart changes nothing, and a repeat never extends the TTL. The target is under a second
from the Node update to the NodeClaim delete (`scarpenter_interruption_reaction_seconds`).

The controller needs no permissions beyond the ones in [Permissions](#permissions): it patches Nodes (SCarpenter's ClusterRole), and patches and deletes
NodeClaims, patches `nodeclaims/status` and creates Events through Karpenter core's role.

### The poller

For a vendor whose driver implements [`InterruptionPoller`](driver-contract.md#interruption-poller-and-the-reason-an-instance-is-gone)
the controller runs one poll loop per (vendor, region) that has a spot NodeClaim of an SCNodeClass, shared by all its instances, and stops
it when the last one is gone (the set is checked every 5 seconds; the poll interval is the driver's). A notice is matched to its NodeClaim by the
`instance-ref` annotation and written to the Node as the three interruption annotations with source `vendor-api`. A Node that has a
`SpotInterrupted` notice already (the node agent's) is left alone; any other value there is replaced, so that a node cannot hide the vendor's
notice behind one the controller ignores. A notice for a NodeClaim without a Node yet is kept in memory and written once the Node exists, even if
the vendor no longer shows it, and dropped when the NodeClaim is gone. The source grants nothing: a node can write `vendor-api` itself, and both
sources get the same checks (reason, spot only, the deadline clamped to the vendor's window); it only labels the record. The controller then reacts exactly as it does to the node
agent's annotation, so the record looks the same whichever path found it. A failed poll is counted in
`scarpenter_interruption_poll_errors_total` and the loop waits twice the interval, doubling per failure up to 5 minutes (throttling
included); it never fails the controller. `--interruption=false` turns the poller off with the controller.

### Silent reclaim

A vendor may take an instance without a notice, or the notice may be missed. Core's NodeClaim garbage collection deletes a Registered NodeClaim
whose instance is gone once its Node is not Ready (about a minute after the node monitor notices, plus up to two minutes); `Get` and `List`
already treat `terminating` and `terminated` as gone. When the first `CloudProvider.Delete` of a NodeClaim finds the instance gone (terminating or
terminated) and core is not disrupting the NodeClaim on purpose (core sets `DisruptionReason` first), the instance went without SCarpenter: core
sets the `InstanceTerminating` condition after every `Delete` that did not fail, so a later `Delete` (after SCarpenter deleted the instance, also
across a restart) does not report it; SCarpenter also remembers the instances it deleted for an hour, for the moment before core stores the
condition. The controller records it, once, for a NodeClaim that has no interruption annotation:

| NodeClaim | Reason | Source | What is recorded |
|---|---|---|---|
| spot, the driver says the vendor reclaimed it (`Instance.GoneReason`) | `SpotInterrupted` | `vendor-api` | the `Interrupted` condition, the offering marked unavailable, the count, the Event |
| spot, no word from the driver | `ReclaimedWithoutNotice` | `gc` | the same, with that reason |
| on-demand | `InstanceVanished` | `gc` | an Event and the count only (the condition is for spot) |

`DisruptionReason` is not set: the node is already drained when this runs. A spot instance that crashed but still exists is not touched
(node repair is core's). The record is made when `Delete` runs, after the drain, so it appears late; the Event and the count last after the
NodeClaim is gone.

**Events**: `SpotInterrupted` (Warning, Node and NodeClaim) with the type, zone, deadline and lead time in the message; `InterruptionIgnored`
(Warning, Node and NodeClaim, once per hour per object); `ReclaimedWithoutNotice` and `InstanceVanished` (Warning, NodeClaim; a reclaim the driver
names is a `SpotInterrupted` Event on the NodeClaim).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_interruptions_total` | counter | `vendor`, `instance_type`, `zone`, `reason`, `source` | Interrupted nodes (`reason` `SpotInterrupted`, `ReclaimedWithoutNotice` or `InstanceVanished`; `source` `instance-metadata`, `vendor-api` or `gc`): the durable count a workload should use (Events expire after an hour). |
| `scarpenter_interruption_lead_seconds` | histogram | `vendor`, `source` | The deadline minus the time the controller saw the notice. |
| `scarpenter_interruption_reaction_seconds` | histogram | | From the controller seeing the notice to the NodeClaim delete. |
| `scarpenter_interruption_poll_errors_total` | counter | `vendor`, `region` | Failed polls of a vendor's API for interruption notices. |

## Catalog

The catalog reads Keeper for the vendors and regions of the SCNodeClasses
(`--keeper-url`, `--keeper-api-key-file`, `--keeper-cache-dir`, `--catalog-refresh-interval`,
`--catalog-max-age`) and is described in
[Labels and offerings](labels-and-offerings.md). `Operator.NewComponents` builds it
from those flags with the driver facts (zones, root disk, profiles, launchable
types), the quota tracker and the unavailable-offerings cache, and runs it in the
manager.

A refresh pins one Keeper data version (`/healthcheck`), reads the manifest
and the snapshot files of that version (from the on-disk copy when
`--keeper-cache-dir` is set, else downloaded), and reads `/benchmark_configs`
live when a metric or selection uses benchmarks. When Keeper still serves the
version of the current catalog for the same scopes and queries, a refresh costs
the one `/healthcheck` request.

**A 401 is never retried** (Keeper charges a rejected request 11 credits): the
refresh stops at the first one and keeps the previous catalog. It logs one error
line, `Keeper needs an API key`, with `keeperURL`, `keyConfigured` and a pointer
to the "+doc+" section, sets `KeeperAuthorized=False`, and the next scheduled
refresh tries once more. Without any catalog, `CatalogReady` is `False` with
reason `KeeperAPIKeyRequired`.

Metrics (Prometheus, on the metrics port):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_catalog_age_seconds` | gauge | `vendor` | Seconds since the catalog snapshot was read from Keeper. |
| `scarpenter_catalog_refresh_errors_total` | counter | | Failed refreshes; the last good snapshot stays in use. |
| `scarpenter_catalog_instance_types` | gauge | `vendor` | Instance types with a price in scope. |
| `scarpenter_catalog_offerings` | gauge | `vendor` | Priced offerings (price rows in scope, before the driver's zone mapping). |
| `scarpenter_catalog_dropped_types` | gauge | `nodeclass`, `vendor`, `reason` | Instance types the NodeClass's catalog leaves out at build time, by [reason code](labels-and-offerings.md#why-a-type-or-an-offering-is-not-offered): `NoPrice`, `RegionNotAllowed`, `NoOffering` and the GPU reasons ([GPU types](labels-and-offerings.md#gpu-types)); at most a dozen series per class and vendor. |
| `scarpenter_catalog_data_published_timestamp_seconds` | gauge | | Unix time Keeper published the data version of the snapshot (`SC-Data-Published-At`): the age of the data, not of the read. |
| `scarpenter_catalog_keeper_unauthorized` | gauge | | `1` after Keeper answered a refresh with 401, `0` after a successful snapshot read. |

At V(1) (`karpenter.logLevel: debug`) the catalog logs every dropped type and unavailable offering with its
reason code after a restart and every change afterwards (`offered types changed`);
see [why a type or an offering is not offered](labels-and-offerings.md#why-a-type-or-an-offering-is-not-offered).
The offered types controller adds `scarpenter_catalog_offerings_by_state` and, with a flag, a per-offering gauge ([Metrics](#metrics)).

At start-up, before core's controllers are built, the controller lists the
SCNodeClasses and registers their `metric-<m>` keys, and all `cpu-flag-<flag>`
keys, with core (`Operator.RegisterLabelKeys`, logged as `registered the label
keys of the SCNodeClasses`). The controller needs permission to list
SCNodeClasses.

Then, still before the manager (and with it the health endpoints and the leader
election) starts, it loads the catalog for those SCNodeClasses
(`Operator.WarmCatalog`, logged as `catalog loaded before the start`), so that
core's cost tracking finds the prices of the existing NodeClaims when its
controllers start; without it core logs `offering price unknown` for each one.
It waits at most 30 s; on an error or the timeout it logs `the catalog did not
load before the start, starting anyway` and the catalog's refresh loop takes
over. Every replica does this, a standby too: its snapshot (and
`scarpenter_catalog_age_seconds`) then ages until it becomes the leader.

## Development builds

Binaries built with `-tags scarpenter_dev` add the fake driver and its flags
(all `SCARPENTER_*` environment fallbacks): `--fake-catalog-vendor` (default
`aws`: the Keeper vendor whose catalog the fake serves, under that vendor ID),
`--fake-config-namespace` (`scarpenter-system`), `--fake-config-name`
(`scarpenter-fake`), `--fake-state-name` (`scarpenter-fake-state`), `--fake-resources-name`
(`scarpenter-fake-resources`), `--fake-launch-delay` (`0`) and `--fake-min-notice` and `--fake-max-notice` (empty: the
[interruption notice window](driver-contract.md#interruption-notice-window) of the impersonated vendor). Details: [the fake driver](driver-contract.md#the-fake-driver).
The qemu driver adds `--qemu-state-dir`, `--qemu-cache-dir`, `--qemu-log-dir`,
`--qemu-bridge`, `--qemu-launch-delay` and `--qemu-notice-url` (below). The NodeClass controller is the same as in production. Development builds also
expose the gauges `scarpenter_fake_state_revision` and
`scarpenter_fake_state_published_revision` (the revision of the fake's in-memory
state and of its state ConfigMap).

### qemu driver flags

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--qemu-state-dir` | `SCARPENTER_QEMU_STATE_DIR` | `<user cache dir>/scarpenter-qemu/state` | One directory per instance (record, disk images, Ignition file, serial log) and the records of managed networks. State survives restarts of the controller. |
| `--qemu-cache-dir` | `SCARPENTER_QEMU_CACHE_DIR` | `<user cache dir>/scarpenter-qemu/cache` | Downloaded Flatcar releases (`flatcar/<channel>/<arch>/<version>`), verified against the release's digest file; honours `--flatcar-base-url`. |
| `--qemu-log-dir` | `SCARPENTER_QEMU_LOG_DIR` | empty | If set, the serial log of every deleted instance is moved here (`<nodeclaim>-<id>.serial.log`). |
| `--qemu-bridge` | `SCARPENTER_QEMU_BRIDGE` | `scbr0` | The bridge of the default network. |
| `--qemu-launch-delay` | `SCARPENTER_QEMU_LAUNCH_DELAY` | `0` | Test aid: how long `CreateInstance` waits after the VM has started before it returns (the L4 scenario that kills the controller in that window). |
| `--qemu-notice-url` | `SCARPENTER_QEMU_NOTICE_URL` | empty | Test aid: base URL of the notice server that the [node agent](node-bootstrap.md#the-interruption-agent) of a spot qemu instance polls (`<url>/<node name>`); a plain http(s) URL. Empty renders no agent (the L4 interruption scenario). |

## CloudProvider

The CloudProvider is named `scarpenter` and supports the `SCNodeClass` kind. It
is vendor-neutral on top of [the driver contract](driver-contract.md).

`Create` resolves the vendor settings of each candidate (driver defaults, the
vendor block, the profiles the NodeClaim's requirements select; see [node
settings](node-settings.md)), orders the candidates by price plus the price delta
of the settings, and for each attempt runs the driver's `Prepare` (vendor
resources, placement groups, volumes, with the placement run key) before the
vendor call, renders the user-data for that attempt (zone, instance type, the
type's kube-reserved, the device hints), and stamps the profile labels, the run
key, the price (`price-incomplete` where the price misses components) and the
profile hashes on the NodeClaim. A vendor resource that is missing or forbidden
fails the candidate (`Unsupported`, code `ResourceMissing` or `ResourceForbidden`)
without marking the type or the zone, skips the other candidates of the region
and makes the region's offerings unavailable. A replayed idempotency key that
returns another instance than the attempted offering moves the quota reservation
to the returned instance.

- `GetInstanceTypes` returns the catalog's instance types for the NodePool's
  SCNodeClass, pre-filtered by the NodePool's requirements on vendor, region,
  zone, architecture, capacity type and instance type. Offerings are
  unavailable when the unavailable-offerings cache or the quota tracker says so.
- `Create` computes the candidate offerings of the NodeClaim, orders them by
  price (ties: larger quota headroom, instance type name, zone, region, capacity
  type), skips those without quota headroom without calling the vendor, and
  tries the others until one launches or `--max-launch-attempts` vendor calls
  failed. Error classes: `InsufficientCapacity` marks the offering unavailable
  and continues; `Unsupported` marks the type in the zone, `ZoneUnusable` the
  zone; `QuotaExceeded` triggers a quota refresh of the region and marks the quota bucket the vendor
  counts against (the region and capacity type for a vendor without a bucket
  mapping) unavailable for the TTL, also when the tracker showed headroom; a NodeClaim of the vendor and region confirmed gone
  clears the mark (an account-wide quota error, `Error.AccountWide`, marks the capacity type
  unavailable in every region of the vendor); `DailyLimit` marks the vendor's region unavailable
  until the vendor's reset time (the next 00:00 UTC when the driver gives none, 5 min within 5 min after it), whatever is deleted; `Retryable` is returned as is (core retries); `Fatal` returns a
  `CreateError` and an Event. When nothing launched it returns
  `InsufficientCapacityError` and core deletes the NodeClaim, except when there are
  no candidates because the catalog is not ready (a controller that has just
  restarted and has not read its first snapshot, a stale one): that is a
  `CreateError` (`CatalogNotReady`) that core retries, because a replayed launch
  of a NodeClaim whose instance exists must not delete the NodeClaim and orphan
  the instance (found by the L4 scenario Q04). The NodeClaim UID
  is the idempotency key. The bootstrap token is minted once per NodeClaim (a retry
  gets the same one) and the user-data rendered once per vendor, region and
  architecture ([node bootstrap](node-bootstrap.md)).
- Provider ID: `scarpenter://<vendor>/<region>/<nodeclaim>`. Annotations on the
  launched NodeClaim: `scarpenter.sparecores.com/instance-ref`, `nodeclass-hash`,
  `nodeclass-hash-version`, `price` (the `price-incomplete` annotation comes with
  the pricing deltas of plan 06). Labels: every single-valued requirement of the
  instance type and offering, the node name as `kubernetes.io/hostname`, and the
  three profile labels (`default` until plan 06).
- A deadline (`context.DeadlineExceeded`, not a cancellation or another Retryable error) of the launch's
  preparation or the image resolution, when no earlier attempt of the NodeClaim may have requested an instance (its
  bootstrap token does not exist yet), marks the region `RegionUnreachable` for 3 minutes (a class that has a single
  region pauses for 3 minutes after one timeout), logs and publishes a `LaunchFailed` Event
  naming the failed call, and the same Create tries the next candidate. A Retryable error of the create call
  itself is unchanged: the token is kept and the launch is retried (an instance may exist). When every candidate
  fails in the preparation, Create fails with `InsufficientCapacity` and its message. No new candidate starts
  3 minutes after the Create began (core deletes an unlaunched NodeClaim after 5 minutes, a create call takes up to 90 s).
- `Get` and `List` read through a 30 s cache of the cluster's instances
  (`Create` and `Delete` update it); a miss asks the driver by NodeClaim tag.
  A driver that calls the vendor per region (`vendors.RegionLister`: AWS, Alibaba Cloud, OCI, OVH) is asked one
  region at a time, up to 8 in parallel; the others once for all regions. A region that fails keeps the
  instances of the previous listing (core's garbage collector deletes the registered NodeClaims missing from
  `List`, so a slow region must not make them look gone), is logged and is not asked again for 3 minutes. A region
  that was never listed counts as empty, unless a NodeClaim of the vendor lives in it: then the listing fails. The
  cache is fresh from the end of the listing.
  Terminated and terminating instances are `NodeClaimNotFound`.
- `Delete` terminates by the `instance-ref` annotation, else by tag lookup, and
  removes the bootstrap token. It returns `NodeClaimNotFound` once the instance
  is gone. An instance that is terminating (the vendor is deleting it, or it is
  stopped or powered off, which every driver reports as terminating) is left alone
  for `--terminating-grace` from the first time `Delete` saw it, then deleted again
  (`DeleteInstance` is idempotent), and again after each further grace.
- `IsDrifted` returns `NodeClassDrift` when the NodeClaim's `nodeclass-hash`
  differs from `status.vendors.<vendor>.hash`.

`RepairPolicies` lists `Ready=False` and `Ready=Unknown` for ten minutes, both
with the action `ReplaceNode`. Core's node repair is an alpha feature and off by
default (`karpenter.featureGates` of the chart stays empty); with its
`NodeRepair` gate on, the controller hands core the validated policies and
core replaces such nodes at the pace of the NodePool's disruption budgets. A
node that never initializes is deleted by the initialization timeout instead,
whether repair is on or not (see [initialization timeout](#initialization-timeout)).

Metrics:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_launches_total` | counter | `vendor`, `region`, `capacity_type`, `result` | Vendor launch calls; `result` is `success` or the error class. |
| `scarpenter_launch_skipped_total` | counter | `vendor`, `region`, `capacity_type`, `reason` | Candidates passed over without a vendor call (`no-quota`, `unavailable`, `quota-exceeded`, `image`). |
| `scarpenter_launch_attempts` | histogram | `result` | Vendor calls per `Create`. |
| `scarpenter_create_duration_seconds` | histogram | `result` | Duration of `Create`. |

Events (on the SCNodeClass): `LaunchFailed` (a `Fatal` vendor error),
`ImageResolutionFailed`. The NodeClass controller, the bootstrap token controller
and the serving certificate approver add `ArtifactsNotResolved`,
`ValidationFailed`, `VendorNotReady`, `RestartRequired`, `MetricInvalid`,
`DeprecatedHardening` (Warning: the NodeClass still sets `spec.bootstrap.hardening`, which has no effect),
`BootstrapTokenDeleted` (on the NodeClaim; also `GarbageCollected` on
the NodeClass for orphan tokens) and `ServingCertificateApproved` /
`ServingCertificateDenied` (on the certificate signing request); see
[node bootstrap](node-bootstrap.md).

### Launch policy

`spec.launch` of an SCNodeClass overrides the controller's launch flags for the
launches of that class. Every field is optional; an unset one keeps the flag.
Changing it is not drift.

| Field | Flag it overrides | Meaning |
|---|---|---|
| `maxAttempts` | `--max-launch-attempts` | Vendor calls one `Create` may make; at least 1. |
| `maxDuration` | none | Wall-clock budget of one `Create`: once spent, no further candidate is tried and `Create` returns `InsufficientCapacityError`. It bounds when an attempt starts, not when it ends: the worst case is `maxDuration` plus one vendor call (a GCP launch may wait for its operation), and the first candidate is always tried. A `Retryable` error is requeued by Karpenter and starts a fresh budget. Greater than 0 and at most `4m30s`, to stay under Karpenter's 5 minute launch timeout. |
| `initializationTimeout` | none | How long a registered node may stay uninitialized before its NodeClaim is deleted, see [initialization timeout](#initialization-timeout). Default `1h`; `0s` disables it; at most `24h`. |
| `insufficientCapacityTTL` | `--unavailable-insufficient-capacity-ttl` | How long the class stays away from an offering that had no capacity: greater than 0 and at most `24h`. It can extend the flag's TTL for this class, never shorten it, see below. A change applies to the marks made after it; the existing ones keep their expiry. |

An `InsufficientCapacity` mark is shared by every class and lasts the flag's TTL
(3m): a known stockout protects all of them. When the launching class sets a
longer `insufficientCapacityTTL`, `Create` also records a mark of that class
alone until now plus the TTL. The class's candidate filtering and its offered
listing see both marks; the other classes only the shared one, so they try the
offering again after the flag's TTL. A TTL shorter than the flag's changes
nothing. The class's own marks are dropped when the class is deleted.

An error the driver marks `vendors.Error.RegionWide` (GCP
`REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`) says the region is out of
capacity for the type: it is marked unavailable in every zone of the region for
the capacity type, and the other zones of the region are not tried.

When the attempts end without a launch, the `InsufficientCapacity` message is
the last error followed by a summary:
`tried 3 offerings in 2 regions / 3 zones; classes: InsufficientCapacity:2,QuotaExceeded:1; stopped by: maxAttempts`.
`stopped by` is `maxAttempts`, `maxDuration` or `no candidates left`.

## Quota

The quota tracker reads, for drivers with the `QuotaReporter` capability, limits
and usage of every bucket per region in scope (the regions the SCNodeClasses
allow, or every region of the driver) and account-wide, and for drivers with
`OfferingLister` the launchable types per zone. An offering is unavailable when a
bucket it consumes has less headroom than one instance needs; launchable
types are dropped from the offerings. Headroom is `limit - used - reserved`;
`reserved` is the in-flight ledger of launches not yet visible in `used` (released
on failure, when a refresh counts the instance, or after 10 minutes). Refreshes
run every `--quota-refresh-interval` (limits every `--quota-limits-refresh-interval`),
and, debounced to 10 s, after a quota error and after an own launch or
termination. A region whose refreshes fail for `--quota-max-staleness` counts as
unknown quota (the reactive path). Types the driver cannot map have unknown
quota and are never blocked.

When every allowed offering of a (vendor, region, capacity type) is blocked, the
controller sets `status.vendors.<vendor>.quota` (region, capacity type, the
limiting bucket, used, limit, reason) and the condition `QuotaAvailable=False`
with reason `QuotaExhausted`, or `QuotaZero` when every listed limit is 0 (it
resolves only with a quota increase); otherwise `QuotaAvailable=True`. Warning
Events `QuotaExhausted` and `QuotaZero` (deduplicated for 10 minutes) go to the
SCNodeClass. With one allowed region and no headroom the pods stay Pending and
nothing is launched; when a refresh shows headroom, core's next scheduling round
(it re-evaluates pending pods every 10 s) provisions them, with no nudge from
SCarpenter. The status is written by server-side apply with field manager
`scarpenter-quota`, which owns only these fields. One apply per class carries the
fields of every vendor of the class, and nothing is written while they are
unchanged; a vendor that leaves the class loses its quota fields.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_quota_limit` | gauge | `vendor`, `region`, `bucket` | Limit of the bucket (`region` is empty for account-wide buckets). |
| `scarpenter_quota_used` | gauge | `vendor`, `region`, `bucket` | Usage of the whole account. |
| `scarpenter_quota_reserved` | gauge | `vendor`, `region`, `bucket` | In-flight reservations. |
| `scarpenter_quota_refresh_age_seconds` | gauge | `vendor`, `region` | Seconds since the last successful refresh. |
| `scarpenter_quota_refresh_errors_total` | counter | `vendor`, `region` | Failed quota or launchable refreshes. |

## Garbage collection

Every `--gc-interval` (leader only) the collector lists the instances tagged with
this cluster in the regions in use (allowed by a NodeClass, or holding a
NodeClaim) and deletes those whose NodeClaim does not exist (or that lack the
`nodeclaim` tag) and that are older than `--gc-grace-period`. An instance without a
NodeClaim that has been `terminating` for longer than `--terminating-grace` (a
stopped or powered-off instance, which no vendor deletes by itself) is deleted again,
with the reason `Terminating`; while its NodeClaim exists, Karpenter core deletes the
NodeClaim (`Get` does not return it) and `Delete` deletes the instance after the
grace. Bootstrap-token
Secrets (`kube-system`, label `scarpenter.sparecores.com/nodeclaim`) of missing
NodeClaims are deleted the same way, with a `GarbageCollected` Event on the
SCNodeClass of the token's `scarpenter.sparecores.com/nodeclass` label. Managed
vendor resources are swept after the instances and tokens by their lifecycle
(`perUse`, `idleTTL`, `retain`) in the driver's dependency order, only when they
carry both the deterministic name and the ownership marker, never those of `use`;
the metrics and Events are in [vendor resources](vendor-resources.md#garbage-collection). A driver whose
launch makes resources of its own besides the instance (the Azure NIC and public IP,
`vendors.LeftoverCollector`) is then asked, per region and not in dry-run, to delete those of
this cluster that name a NodeClaim that does not exist and that nothing uses; the NodeClaim
check is a live read made after the driver's listing, so a launch in flight keeps its
resources. These deletions are logged and counted (kind `launch-leftover`), without an Event. Nodes with no NodeClaim are deleted
too (kind `node`, reason `InstanceNotFound`): a machine that keeps running after its delete can register its Node again, and
core removes only Nodes that have a NodeClaim. A Node is deleted only when its provider ID is of an enabled vendor, no
NodeClaim has that provider ID, it is older than `--gc-grace-period`, and the driver's lookup of the instance by the NodeClaim
name in the provider ID answers not found; an instance that exists, a failed lookup, or a Node of an instance type the driver's lookup cannot see (a Vultr bare-metal
plan while the registration timeout is too short to list bare metal) keeps the Node. The reaper is a backstop: deleting a Node
does not stop the kubelet of a machine that still runs, which registers it again. An orphan instance whose driver answers that it
is stopping before the delete (Vultr bare metal) is neither an error nor held back by the terminating grace once it is stopped. Every deletion is logged with its reason and published as the Normal Event
`GarbageCollected` on the SCNodeClass of the instance's `nodeclass` tag, else on
the controller Deployment (`SCARPENTER_DEPLOYMENT_NAME`, `POD_NAMESPACE`).

### Public addresses

The same listing carries each instance's public IPv4 and IPv6 address (`Instance.PublicIPv4`, `PublicIPv6`). For an instance
whose NodeClaim names a Node (`status.nodeName`), the collector sets the annotations `scarpenter.sparecores.com/public-ipv4`
and `scarpenter.sparecores.com/public-ipv6` on the Node and on the NodeClaim, updates them when the address changes, and
removes one the instance lost. A family without an address gets no annotation; an empty value is never written. They are
annotations because there is no cloud-controller-manager: the kubelet owns `status.addresses` and replaces what another
writer puts there, so an `ExternalIP` would not stay. The write is not part of `--gc-dry-run`.

How the drivers fill them, from what they already read (no extra call, except OCI):

| Vendor | Public IPv4 | Public IPv6 |
|---|---|---|
| AWS | the instance's public IP and the public IPs associated with its interfaces | the interfaces' IPv6 addresses |
| GCP | the NAT IPs of the interfaces' access configs | the external IPv6 of the IPv6 access configs |
| Azure | the address of the public IP resource of the NIC's IP configuration | the same, for an IPv6 public IP |
| Hetzner | the server's public IPv4 | `<prefix>::1` of the server's /64 |
| OCI | the public IP of the instance's VNICs | the IPv6 addresses of the VNICs |
| OVH | the IPv4 on the `Ext-Net` network | the IPv6 on the `Ext-Net` network |
| Alibaba Cloud | the public IP and the EIP | the IPv6 addresses of the interfaces |
| UpCloud | the address with public access | the same, for IPv6 |
| Vultr (VM and bare metal) | `main_ip` (never the VPC's `internal_ip`) | `v6_main_ip` |

A driver ignores a private, loopback, link-local or unspecified address (`vendors.Instance.SetPublic`). An address
the vendor assigns only after boot appears with the first sweep that sees it. The OCI listing carries no addresses (reading them takes
a call per VNIC), so the collector asks for the instance itself, while the Node has no public address annotation:
a node without a public address costs that read on each sweep.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_gc_deletions_total` | counter | `vendor`, `kind`, `reason`, `result` | `kind`: `instance`, `bootstrap-token`, `node` or `launch-leftover`; `reason`: `NodeClaimNotFound`, `NoNodeClaimTag`, `Terminating` or `InstanceNotFound`; `result`: `deleted`, `dry-run`, `error`. |
| `scarpenter_gc_sweep_duration_seconds` | histogram | | Duration of a sweep. |
| `scarpenter_gc_last_sweep_timestamp_seconds` | gauge | | Time of the last sweep. |

## Events

| Reason | Type | On | Meaning |
|---|---|---|---|
| `LaunchFailed` | Warning | SCNodeClass | A `Fatal` vendor error ended a launch. |
| `ImageResolutionFailed` | Warning | SCNodeClass | A boot image could not be resolved. |
| `ImageCopying`, `ImageCopyFailed` | Normal / Warning | SCNodeClass | An image of the class is being copied into a region, or its copy failed; one per class, architecture and outcome ([Image copies](image-copies.md#events)). |
| `ImageCopyDeleted` | Normal | the controller's Deployment | The collector deleted an image copy ([Image copies](image-copies.md#deletion-rules)). |
| `OfferingRejected` | Warning | SCNodeClass | The vendor rejected the launch of an offering (`InvalidOffering`, with the vendor's message); it is unavailable for an hour. |
| `ArtifactsNotResolved`, `ValidationFailed`, `VendorNotReady`, `MetricInvalid`, `RestartRequired` | Warning / Normal | SCNodeClass | The NodeClass controller's findings ([conditions](#scnodeclass-status-conditions)). |
| `UserDataTooLarge` | Warning | SCNodeClass | The trial rendering of a spot node with the largest GPU part, the largest storage setup and the agent exceeds the vendors' user-data limit: launches of such types are refused one by one ([size](node-bootstrap.md#the-interruption-agent)); the NodeClass stays valid. |
| `UserDataTooLarge` | Warning | SCNodeClass | The trial rendering of a spot node with the largest GPU part, the largest storage setup and the agent exceeds the vendors' user-data limit: launches of such types are refused one by one ([size](node-bootstrap.md#the-interruption-agent)); the NodeClass stays valid. |
| `IPv6OnlyEgress` | Warning | SCNodeClass | IPv6-only nodes in a network you bring, with the default sysext source (no IPv6): the IPv4 egress of that network must reach it. |
| `QuotaExhausted`, `QuotaZero` | Warning | SCNodeClass | No headroom in a quota bucket (deduplicated for 10 minutes). |
| `BootstrapTokenDeleted` | Normal | NodeClaim | The node registered; its token was deleted. |
| `GarbageCollected` | Normal | SCNodeClass, else the Deployment | An orphan instance, bootstrap token or resource was deleted ([garbage collection](#garbage-collection)). |
| `ServingCertificateApproved`, `ServingCertificateDenied` | Normal / Warning | CertificateSigningRequest | The serving certificate approver's decision ([node bootstrap](node-bootstrap.md#the-serving-certificate-approver)). |
| `ResourceCreated`, `ResourceAdopted`, `ResourceDeleted` | Normal | SCNodeClass, else the Deployment | A managed vendor resource was created, adopted after a restart, or deleted ([vendor resources](vendor-resources.md#metrics-and-events)). |
| `ResourceCreateFailed`, `ResourceDeleteFailed` | Warning (deduplicated for 10 minutes) | SCNodeClass, else the Deployment | A creation or deletion failed; deletions retry with backoff. |
| `ResourcesUnavailable` | Warning | SCNodeClass | A region lacks a resource the NodeClass needs, so its offerings are unavailable. |
| `SpotInterrupted`, `InterruptionIgnored` | Warning | Node, NodeClaim | A spot node's reclaim was announced; a notice was ignored ([Interruption](#interruption)). |
| `RetainedUntil`, `ReleasedAtPeriodEnd`, `ReleasedUnderPressure`, `ReleaseBlockedByBudget`, `RetentionSkipped`, `BillingStartUnknown` | Normal / Warning | Node | Billing retention ([Billing retention](#billing-retention)). |

## Metrics

Besides the metrics of Karpenter core, the controller serves, on the metrics port:

| Metric | Section |
|---|---|
| `scarpenter_catalog_age_seconds`, `scarpenter_catalog_refresh_errors_total`, `scarpenter_catalog_instance_types`, `scarpenter_catalog_offerings`, `scarpenter_catalog_dropped_types`, `scarpenter_catalog_data_published_timestamp_seconds`, `scarpenter_catalog_keeper_unauthorized` | [Catalog](#catalog) |
| `scarpenter_launches_total`, `scarpenter_launch_skipped_total`, `scarpenter_launch_attempts`, `scarpenter_create_duration_seconds` | [CloudProvider](#cloudprovider) |
| `scarpenter_quota_limit`, `scarpenter_quota_used`, `scarpenter_quota_reserved`, `scarpenter_quota_refresh_age_seconds`, `scarpenter_quota_refresh_errors_total` | [Quota](#quota) |
| `scarpenter_gc_deletions_total`, `scarpenter_gc_sweep_duration_seconds`, `scarpenter_gc_last_sweep_timestamp_seconds` | [Garbage collection](#garbage-collection) |
| `scarpenter_interruptions_total`, `scarpenter_interruption_lead_seconds`, `scarpenter_interruption_reaction_seconds` | [Interruption](#interruption) |
| `scarpenter_billing_*` (11 metrics) | [Billing retention](#billing-retention) |
| `scarpenter_image_copies_started_total`, `scarpenter_image_copies_completed_total` (`vendor`, `region`), `scarpenter_image_copies_failed_total` (`vendor`, `region`, `code`: the vendor's error code, `CopyFailed` or `CopyTimeout`), `scarpenter_image_copy_duration_seconds` (histogram, `vendor`: start to available), `scarpenter_image_copies_deleted_total` (`vendor`, `reason`: `Idle`, `PrimaryAvailable`, `Unpinned`, `Duplicate`, `Failed`), `scarpenter_image_copies` (gauge, `vendor`, `state`: `queued`, `pending`, `available`, `failed`) | [Image copies](image-copies.md#metrics) (ADR-032, plan 29) |
| `scarpenter_resource_operations_total` (`vendor`, `kind`, `region`, `operation` `create`, `adopt` or `delete`, `result` `ok`, `error` or `dry-run`) and `scarpenter_resources_managed` (`vendor`, `kind`, `region`: the managed resources the last sweep found) | [Vendor resources](vendor-resources.md#metrics-and-events) |

| `scarpenter_catalog_offerings_by_state` (`nodeclass`, `vendor`, `state`: `available` or an offering-level reason code, at most ten series per class and vendor) | [the offered summary](labels-and-offerings.md#the-offered-summary-in-the-status) |
| `scarpenter_offered_listing_bytes` (`nodeclass`, `vendor`) | compressed bytes of the last [listing](labels-and-offerings.md#the-offered-listing) written |
| `scarpenter_instance_type_offering_available` (`nodeclass`, `instance_type`, `zone`, `capacity_type`; 1 available, 0 listed unavailable), only with `--metrics-offering-detail` | one series per offering: thousands per class and vendor; the [listing](labels-and-offerings.md#the-offered-listing) has the same data |

Development builds add the `scarpenter_fake_state_*` gauges.
