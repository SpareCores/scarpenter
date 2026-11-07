#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the
# order of overview section 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      terminates the instances), if the controller still runs;
#   2. force-terminate anything tagged with the run's cluster and delete what it left
#      (placement groups, security groups, the managed network, the brought subnet and
#      group) with the sweeper, ignoring the age limit;
#   3. check that nothing tagged with the cluster remains: no instance, security group,
#      network interface, placement group, subnet, VPC, gateway, route table or volume
#      (plan 10 scenario 5): the job fails when something does;
#   4. revoke the nodes' auth key and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-aws/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_OUT AWS_REGION
root="${e2e_root}"
failures=0
problem() {
  echo "PROBLEM: $*" >&2
  failures=$((failures + 1))
}

log "1. NodePools and NodeClaims"
if kubectl get --raw /readyz >/dev/null 2>&1; then
  kjson nodepools 2>/dev/null | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=false
  if kubectl -n "${ns}" get deploy scarpenter >/dev/null 2>&1; then
    deadline=$((SECONDS + 300))
    until [[ -z "$(kubectl get nodeclaims -o name 2>/dev/null)" ]]; do
      if [[ "${SECONDS}" -ge "${deadline}" ]]; then
        echo "NodeClaims are left after 300 s: the instances are force-terminated next" >&2
        break
      fi
      sleep 5
    done
  else
    echo "the controller is gone (uninstalled): nothing deletes NodeClaims any more"
  fi
else
  echo "the API server does not answer: going straight to AWS"
fi

log "2. force-terminate and sweep what the cluster tag finds"
left="$(cluster_instances | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} instance(s) were still there and are terminated by the sweeper"
(cd "${root}" && go run ./test/e2e-aws/sweeper -cluster "${E2E_CLUSTER}" -max-age 0 -regions "${AWS_REGION}" -wait 5m) ||
  problem "the sweeper failed"

log "3. nothing tagged with ${E2E_CLUSTER} may remain"
leaks() { # leaks <what> <command printing the IDs>
  local out
  out="$("${@:2}" 2>&1)" || {
    problem "could not list ${1}: ${out}"
    return
  }
  [[ -z "${out}" ]] || problem "${1} left: ${out}"
}
filter=("Name=tag:${ctag},Values=${E2E_CLUSTER}")
leaks instances cluster_instances
leaks "security groups" ec2 describe-security-groups --filters "${filter[@]}" --query 'SecurityGroups[].GroupId' --output text
leaks "placement groups" ec2 describe-placement-groups --filters "${filter[@]}" --query 'PlacementGroups[].GroupName' --output text
leaks subnets ec2 describe-subnets --filters "${filter[@]}" --query 'Subnets[].SubnetId' --output text
leaks VPCs ec2 describe-vpcs --filters "${filter[@]}" --query 'Vpcs[].VpcId' --output text
leaks "internet gateways" ec2 describe-internet-gateways --filters "${filter[@]}" --query 'InternetGateways[].InternetGatewayId' --output text
leaks "route tables" ec2 describe-route-tables --filters "${filter[@]}" --query 'RouteTables[].RouteTableId' --output text
leaks volumes ec2 describe-volumes --filters "${filter[@]}" --query "Volumes[?State!='deleting'].VolumeId" --output text
# The interfaces of terminated instances go away a little later.
deadline=$((SECONDS + 180))
until [[ -z "$(ec2 describe-network-interfaces --filters "${filter[@]}" --query 'NetworkInterfaces[].NetworkInterfaceId' --output text 2>/dev/null)" ]]; do
  if [[ "${SECONDS}" -ge "${deadline}" ]]; then
    leaks "network interfaces" ec2 describe-network-interfaces --filters "${filter[@]}" --query 'NetworkInterfaces[].NetworkInterfaceId' --output text
    break
  fi
  sleep 10
done
# A spot request that is still open (never fulfilled) would come back to life; an active
# one of a terminated instance closes by itself within minutes.
leaks "open spot requests" ec2 describe-spot-instance-requests \
  --filters "${filter[@]}" Name=state,Values=open --query 'SpotInstanceRequests[].SpotInstanceRequestId' --output text

log "4. the auth key and the tailnet"
if [[ -n "${TS_OAUTH_CLIENT_ID:-}" && -n "${TS_OAUTH_SECRET:-}" ]]; then
  ts_revoke_key "${E2E_OUT}/ts-authkey.id" || echo "could not revoke the auth key (it expires after an hour anyway)"
fi
sudo tailscale logout >/dev/null 2>&1 || true

if [[ "${failures}" -ne 0 ]]; then
  echo "teardown: ${failures} problem(s)" >&2
  exit 1
fi
echo "teardown: clean"
