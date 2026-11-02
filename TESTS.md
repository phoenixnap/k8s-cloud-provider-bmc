# Tests

## Automated tests

| Command | What it covers | Needs |
| --- | --- | --- |
| `make test` | Unit tests, against an in-process fake of the PhoenixNAP API | Go |
| `make lint` / `make helm-lint` | golangci-lint; `helm lint --strict` of the chart | golangci-lint, helm |
| `make e2e` | Installs the Helm chart in a local [kind](https://kind.sigs.k8s.io) cluster against a mock PhoenixNAP API ([test/e2e/mockapi](test/e2e/mockapi/main.go)), and checks node initialization and the `type=LoadBalancer` lifecycle. `KEEP_CLUSTER=1` keeps the cluster afterwards. | docker, kind, kubectl, helm |
| `make test-live` | Read-only checks of authentication and scopes against the production API, through the CCM's own clients | `PNAP_CLIENT_ID`, `PNAP_CLIENT_SECRET` |

`make test`, the linters and `make e2e` run in CI.

### Live tests that create billable resources

[phoenixnap/live_test.go](phoenixnap/live_test.go) also has tests that create, and then delete, real resources.
They only run with the `live` build tag and their own opt-in env vars:

```console
# a public network and a /29 IP block, through the full LoadBalancer lifecycle
PNAP_LIVE_LOCATION=... go test -tags live -run TestLiveLoadBalancer -v -timeout 30m ./phoenixnap/
# InstancesV2 against an existing server, which is left untouched
PNAP_LIVE_SERVER_ID=... go test -tags live -run TestLiveInstances -v ./phoenixnap/
# or against a new hourly server, which is deleted at the end
PNAP_LIVE_LOCATION=... PNAP_LIVE_SERVER_TYPE=... go test -tags live -run TestLiveInstances -v -timeout 90m ./phoenixnap/
```

Each test deletes what it created, and reports anything it could not delete in capitals. Afterwards,
`go test -tags live -run TestLiveLeftovers -v ./phoenixnap/` checks the account for anything left behind.

## Manual test plan (legacy)

Parts of this plan were inherited from another provider and describe features this CCM does not have,
such as BGP, metallb and a control plane FIP.

Currently, these are performed manually, but executing each of the steps in each section of this document.

In the next phase, we should create a single `make` target that runs them all. These require a PhoenixNAP account
and token, deploying several real devices.

Eventually, these should be part of the CI flow.

The user flow is expected to be:

```console
export PNAP_CLIENT_ID=<my phoenixnap client ID>
export PNAP_CLIENT_SECRET=<my phoenixnap client secret>
make integration-tests
```

## Prerequisites

In order to test the CCM, we need a Kubernetes cluster with at least some of the nodes - control plane or worker - on
PhoenixNAP, depending on the test. For simplicity sake, all tests will use a cluster, all of whose nodes, both
control plane and worker, are on PhoenixNAP.

It is not necessary to deploy the CCM onto the cluster, as the CCM can run in standalone mode.
This makes the testing part of the lifecycle simpler.

## Tests

The functionality that needs to be tested is as follows:

* node management - provider ID, deletion, metadata
* load balancer service - addition, update, deletion
* control plane FIP

### Node Management

* add new PhoenixNAP server to cluster, check that:
  * node receives provider ID
  * provider ID aligns with server ID in PhoenixNAP
  * node receives appropriate metadata as labels, specifically: type, addresses, location
* remove PhoenixNAP server by deleting, check that:
  * Kubernetes deletes the node

### LoadBalancer Services

As there are different loadbalancer implementations, these require different tests:

#### kube-vip, bgp

1. Start with 0 services of `type=LoadBalancer`
1. add 2 services of `type=LoadBalancer`, check that:
  * FIPs are created, have correct tags
  * BGP is enabled for project
  * BGP is enabled on each node with the services
  * annotations are created on each node, specifically, multiple of: local ASN, peer ASN, local IP, peer IP
1. delete 1 service of `type=LoadBalancer`, check that:
  * FIP is removed
  * annotations remain
  * FIP for 2nd service remains
1. delete 2nd service of `type=LoadBalancer`, check that:
  * FIP is removed
  * annotations are removed

#### metallb

1. Start with 0 services of `type=LoadBalancer`
1. add 2 services of `type=LoadBalancer`, check that:
  * FIPs are created, have correct tags
  * BGP is enabled for project
  * BGP is enabled on each node with the services
  * metallb configmap is modified to include:
    * an addresses entry for each service
    * a node entry for each combination of node and upstream peer
1. delete 1 service of `type=LoadBalancer`, check that:
  * FIP is removed
  * configmap remains, with address entry for 1st service removed
  * FIP for 2nd service remains
1. delete 2nd service of `type=LoadBalancer`, check that:
  * FIP is removed
  * configmap is emptied

### Control Plane FIP

This requires a control plane of 3 nodes; workers are not necessary.

1. Create the control plane nodes.
1. Create the FIP, tagged with a unique tag.
1. Start CCM with the tag passed.
1. Check that the FIP has been attached to one of the control plane nodes.
1. Delete one of the non-FIP-attached nodes; check that the FIP remains attached to the same node.
1. Disable one of the FIP-attached nodes, but do not delete from PhoenixNAP API; check that the FIP is detached from that node and attached to the remaining node.
