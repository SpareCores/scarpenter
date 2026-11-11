# Set up Azure credentials

The controller calls the Azure Resource Manager and needs an identity for that. The nodes it launches get no
cloud credentials. Nothing here has run against a real subscription yet (see
[what is not verified](../reference/azure-driver.md#what-is-not-verified-against-azure-yet)).

- [Choose a mode](#choose-a-mode)
- [The role](#the-role)
- [Workload identity](#workload-identity-recommended)
- [Client secret](#client-secret-the-fallback)
- [AKS and managed identity](#aks-and-managed-identity)

## Choose a mode

| Mode (`vendors.azure.credentials.mode`) | The controller authenticates with | Use when |
|---|---|---|
| `workloadIdentity` (default) | the ServiceAccount's projected token, exchanged by Entra ID for the identity's token | any cluster whose ServiceAccount issuer Azure can reach (AKS, or a self-managed cluster that publishes its issuer). No long-lived secret. |
| `secret` | `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_CLIENT_SECRET` from a Secret you create | the cluster cannot federate. Rotate the secret. |
| `none` | what the SDK's chain finds | the Azure workload identity webhook injects the variables, or the node has a managed identity |

Always set `vendors.azure.subscriptionID` (the chart passes it as `AZURE_SUBSCRIPTION_ID`).

## The role

[`azure-role.json`](../reference/azure-role.json) is a custom role definition: replace the assignable scope with
your subscription (or the resource group of the nodes) and create it with `az role definition create --role-definition
azure-role.json`. Assign it to the identity at the same scope. Register the resource providers
`Microsoft.Compute`, `Microsoft.Network` and `Microsoft.MarketplaceOrdering`. The action list is a design, not yet
checked live.

## Workload identity (recommended)

1. Create a user-assigned managed identity and assign the role.
2. Add a federated credential: issuer is your cluster's ServiceAccount issuer, subject
   `system:serviceaccount:<namespace>:<serviceaccount>`, audience `api://AzureADTokenExchange`.
3. Install with `vendors.azure.enabled=true`, `subscriptionID`, `credentials.workloadIdentity.clientId` and `tenantId`.
   The chart projects the token and sets `AZURE_FEDERATED_TOKEN_FILE`.

## Client secret (the fallback)

Create an app registration, assign the role, put `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_CLIENT_SECRET` in a
Secret and set `credentials.mode=secret` and `credentials.secret.existingSecret`.

## AKS and managed identity

With the AKS workload identity add-on use mode `none` and annotate the ServiceAccount; on an Azure VM with a managed
identity, mode `none` finds it.

## Marketplace terms

The driver never accepts terms. If an image needs them, the NodeClass's `ImagesReady` message names the
command; an account owner runs `az vm image terms accept`.
