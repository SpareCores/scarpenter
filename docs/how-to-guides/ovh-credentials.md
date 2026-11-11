# Set up OVHcloud credentials

The OVHcloud driver talks to the OpenStack APIs (Keystone, Nova, Neutron, Glance, Cinder) of **one Public Cloud
project**: the project is the account scope, and one controller serves one project. This guide creates the
identity the controller uses and wires it into the chart. The driver's behaviour is in the
[OVHcloud driver reference](../reference/ovh-driver.md).

Nothing here has run against OVH yet: the steps are written from OVH's documentation, and the open
questions of [plan 18](../plans/v1/wip/18-ovh.md) (spikes S1 to S8) say what a live run must still confirm.

## Table of Contents

- [A dedicated OpenStack user](#a-dedicated-openstack-user)
- [The Secret](#the-secret)
- [The chart values](#the-chart-values)
- [Regions and quotas](#regions-and-quotas)
- [The IAM service-account mode (not built)](#the-iam-service-account-mode-not-built)
- [Check it](#check-it)

## A dedicated OpenStack user

Create a user of the Public Cloud project **for SCarpenter only** (not the one of your infrastructure tool), in the
Control Panel under *Public Cloud, your project, Users & Roles*, or through the OVH API. It needs exactly these
five OpenStack roles, which are the rights the driver uses:

| Role | What the driver does with it |
|---|---|
| Compute Operator (`computeOperator`) | Nova: flavors, servers (create, list, delete), availability zones, limits, server groups |
| Network Operator (`networkOperator`) | Neutron: the `Ext-Net` network the nodes attach to |
| Network Security Operator (`networkSecurityOperator`) | Neutron: security groups and their rules |
| Image Operator (`imageOperator`) | Glance: the driver only reads images (OVH documents no read-only image role) |
| Volume Operator (`volumeOperator`) | Cinder: volume types, and the volumes of `bootFromVolume` and data volumes |

The exact least-privilege set is not verified (spike S6). Keep the user's password in your secret store; it
is the only credential: there is no workload identity from the cluster to OVH.

## The Secret

The driver reads the standard OpenStack environment. From the user's `openrc` file, or by hand:

| Variable | Value |
|---|---|
| `OS_AUTH_URL` | `https://auth.cloud.ovh.net/v3` |
| `OS_USERNAME` | the user's name |
| `OS_PASSWORD` | its password |
| `OS_PROJECT_ID` | the project's ID (the OpenStack tenant ID of the Public Cloud project) |
| `OS_USER_DOMAIN_NAME` | `Default` |

```sh
kubectl -n scarpenter create secret generic ovh-openstack \
  --from-literal=OS_AUTH_URL=https://auth.cloud.ovh.net/v3 \
  --from-literal=OS_USERNAME=scarpenter \
  --from-literal=OS_PASSWORD='...' \
  --from-literal=OS_PROJECT_ID=0123456789abcdef0123456789abcdef \
  --from-literal=OS_USER_DOMAIN_NAME=Default
```

The controller starts without calling OVH: it checks only that the variables are set, and authenticates with its
first call (a failure is retried by the next one). The token is renewed when it expires.

## The chart values

```yaml
vendors:
  ovh:
    enabled: true
    credentials:
      mode: secret
      secret:
        existingSecret: ovh-openstack
        # The key names are the defaults (OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_ID,
        # OS_USER_DOMAIN_NAME); set authUrlKey, usernameKey, passwordKey, projectIdKey or
        # userDomainNameKey for another layout.
```

`mode: none` wires nothing, for a deployment that sets the `OS_*` variables itself (`extraEnv`). The chart adds `ovh` to
`--vendors` next to `aws` when both are enabled; `scheduleWait` and `regionAliases` are the driver's two
options ([controller reference](../reference/controller.md)). The NodeClass then needs the
[Flatcar image](ovh-images.md) in every region it names: `vendors.ovh.regions`.

## Regions and quotas

The NodeClass `regions` and the `ids` keys of `resources` are the **names of the project's Keystone catalog**, upper
case (`GRA11`, `SBG5`, `EU-WEST-PAR`), not OVH's newer long names. List them from the API
(`GET /cloud/project/{serviceName}/region`) or the CLI (`openstack catalog show compute`) instead of from a list. A region
has to be activated in the project before it has endpoints. The driver joins them to Keeper's names through
its region table and logs a catalog region it does not know (a row can be added with `vendors.ovh.regionAliases`).

New projects start with low per-region quotas on instances, cores and RAM; the driver reads them
from Nova's absolute limits and stops launching at the limit (`QuotaExceeded`). Raise them in the Control Panel
([increasing quotas](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/increasing-public-cloud-quota)).

## The IAM service-account mode (not built)

OVH recommends service accounts (OAuth2 client credentials) for automation, and OpenStack accepts them through
Keystone federation (`v3oidcclientcredentials`). gophercloud has no OIDC plugin, so the driver would need a small
Keystone client of its own: it is not built (plan 18, open question 3), and the user above is the supported mode.
[`ovh-iam-policy.json`](../reference/ovh-iam-policy.json) is the policy such a service account needs on the project:
the same five roles as `publicCloudProject:openstack:<role>` actions on the project's URN.

## Check it

```sh
kubectl get scnodeclass <name> -o jsonpath='{.status.vendors.ovh.conditions}'
```

`VendorReady` is true when the driver validates the block, `ImagesReady` when every region has the image. An
authentication failure shows as `Unauthorized` in the controller's log and as a `Fatal` error in the
events of the NodeClass.
