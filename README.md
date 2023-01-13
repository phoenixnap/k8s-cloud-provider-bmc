# Kubernetes Cloud Controller Manager for PhoenixNAP

[![GitHub release](https://img.shields.io/github/release/phoenixnap/cloud-provider-pnap/all.svg?style=flat-square)](https://github.com/phoenixnap/cloud-provider-pnap/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/phoenixnap/cloud-provider-pnap)](https://goreportcard.com/report/github.com/phoenixnap/cloud-provider-pnap)
![Continuous Integration](https://github.com/phoenixnap/cloud-provider-pnap/workflows/Continuous%20Integration/badge.svg)
[![Docker Pulls](https://img.shields.io/docker/pulls/phoenixnap/cloud-provider-pnap.svg)](https://hub.docker.com/r/phoenixnap/cloud-provider-pnap/)
![PhoenixNAP Maintained](https://img.shields.io/badge/stability-maintained-green.svg)


`cloud-provider-pnap` is the Kubernetes CCM implementation for PhoenixNAP. Read more about the CCM in [the official Kubernetes documentation](https://kubernetes.io/docs/tasks/administer-cluster/running-cloud-controller/).

This repository is **Maintained**!

## Requirements

At the current state of Kubernetes, running the CCM requires a few things.
Please read through the requirements carefully as they are critical to running the CCM on a Kubernetes cluster.

### Version

Recommended versions of PhoenixNAP CCM based on your Kubernetes version:

* PhoenixNAP CCM version v1.0.0+ supports Kubernetes version >=1.20.0

## Deployment

**TL;DR**

1. Set Kubernetes binary arguments correctly
1. Get your PhoenixNAP client ID and client secret
1. Deploy your PhoenixNAP client ID and client secret to your cluster in a [secret](https://kubernetes.io/docs/concepts/configuration/secret/)
1. Deploy the CCM
1. Deploy the load balancer (optional)

### Kubernetes Binary Arguments

Control plane binaries in your cluster must start with the correct flags:

* `kubelet`: All kubelets in your cluster **MUST** set the flag `--cloud-provider=external`. This must be done for _every_ kubelet. Note that [k3s](https://k3s.io) sets its own CCM by default. If you want to use the CCM with k3s, you must disable the k3s CCM and enable this one, as `--disable-cloud-controller --kubelet-arg cloud-provider=external`.
* `kube-apiserver` and `kube-controller-manager` must **NOT** set the flag `--cloud-provider`. They then will use no cloud provider natively, leaving room for the PhoenixNAP CCM.

**WARNING**: setting the kubelet flag `--cloud-provider=external` will taint all nodes in a cluster with `node.cloudprovider.kubernetes.io/uninitialized`.
The CCM itself will untaint those nodes when it initializes them.
Any pod that does not tolerate that taint will be unscheduled until the CCM is running.

You **must** set the kubelet flag the first time you run the kubelet. Stopping the kubelet, adding it after,
and then restarting it will not work.

#### Kubernetes node names must match the device name

By default, the kubelet will name nodes based on the node's hostname.
PhoenixNAP device hostnames are set based on the name of the device.
It is important that the Kubernetes node name matches the device name.

### Get PhoenixNAP client ID and client secret

To run `cloud-provider-pnap`, you need your PhoenixNAP client ID and client secret that your cluster is running in.
You can generate them from the [PhoenixNAP portal](https://bmc.phoenixnap.com/credentials).
Ensure it at least has the scopes of `"bmc"`, `"bmc.read"`, `"tags"` and `"tags.read"`.

Once you have this information you will be able to fill in the config needed for the CCM.

### Deploy secret

Copy [deploy/template/secret.yaml](./deploy/template/secret.yaml) to someplace useful:

```bash
cp deploy/template/secret.yaml /tmp/secret.yaml
```

Replace the placeholder in the copy with your token. When you're done, the `yaml` should look something like this:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pnap-cloud-config
  namespace: kube-system
stringData:
  cloud-sa.json: |
    {
    "clientID": "abc123abc123abc123",
    "clientSecret": "def456def456def456",
    }  
```


Then apply the secret, e.g.:

```bash
kubectl apply -f /tmp/secret.yaml
```

You can confirm that the secret was created with the following:

```bash
$ kubectl -n kube-system get secrets pnap-cloud-config
NAME                  TYPE                                  DATA      AGE
pnap-cloud-config   Opaque                                1         2m
```

### Deploy CCM

To apply the CCM itself, select your release and apply the manifest:

```
RELEASE=v2.0.0
kubectl apply -f https://github.com/phoenixnap/cloud-provider-pnap/releases/download/${RELEASE}/deployment.yaml
```

The CCM uses multiple configuration options. See the [configuration](#Configuration) section for all of the options.

### Logging

By default, ccm does minimal logging, relying on the supporting infrastructure from kubernetes. However, it does support
optional additional logging levels via the `--v=<level>` flag. In general:

* `--v=2`: log most function calls for devices and facilities, when relevant logging the returned values
* `--v=3`: log additional data when logging returned values, usually entire go structs
* `--v=5`: log every function call, including those called very frequently

## Configuration

The PhoenixNAP CCM has multiple configuration options. These include several different ways to set most of them, for your convenience.

1. Command-line flags, e.g. `--option value` or `--option=value`; if not set, then
1. Environment variables, e.g. `CCM_OPTION=value`; if not set, then
1. Field in the configuration [secret](https://kubernetes.io/docs/concepts/configuration/secret/); if not set, then
1. Default, if available; if not available, then an error

This section lists each configuration option, and whether it can be set by each method.

| Purpose | CLI Flag | Env Var | Secret Field | Default |
| --- | --- | --- | --- | --- |
| Path to config secret |    |    | `cloud-config` | error |
| Client ID |    | `PNAP_CLIENT_ID` | `clientID` | error |
| Client Secret |    | `PNAP_CLIENT_SECRET` | `clientSecret` | error |
| Location in which to create LoadBalancer Floating IPs |    | `PNAP_LOCATION` | `location` | Service-specific annotation, else error |
| Base URL to PhoenixNAP API |    |    | `base-url` | Official PhoenixNAP API |
| Load balancer setting |   | `PNAP_LOAD_BALANCER` | `loadbalancer` | none |
| Kubernetes Service annotation to set IP block location |   | `PNAP_ANNOTATION_IP_LOCATION` | `annotationIPLocation` | `"phoenixnap.com/ip-location"` |
| Kubernetes API server port for IP |     | `PNAP_API_SERVER_PORT` | `apiServerPort` | Same as `kube-apiserver` on control plane nodes, same as `0` |

**Location Note:** In all cases, where a "location" is required, use the 3-letter short-code of the location. For example,
`"SEA"` or `"ASH"`.

## How It Works

The Kubernetes CCM for PhoenixNAP deploys as a `Deployment` into your cluster with a replica of `1`. It provides the following services:

* lists and retrieves instances by ID, returning PhoenixNAP instances
* manages load balancers

### Load Balancers

PhoenixNAP does not offer managed load balancers like [AWS ELB](https://aws.amazon.com/elasticloadbalancing/)
or [GCP Load Balancers](https://cloud.google.com/load-balancing/). Instead, if configured to do so,
PhoenixNAP CCM will interface with and configure loadbalancing using PhoenixNAP IP blocks and tags.

#### Service Load Balancer IP

For a Service of `type=LoadBalancer` CCM will create one using the PhoenixNAP API and assign it to the network,
so load balancers can consume them.

PhoenixNAP's API does not support adding tags to individual IP addresses, while it has full support for tags on blocks.
PhoenixNAP CCM uses tags to mark IP blocks and individual IPs as assigned to specific services.

Each block is given at least 2 tags:

* `usage=cloud-provider-phoenixnap-auto` - identifies that the IP block was reserved automatically using the phoenixnap CCM
* `cluster=<clusterID>` - identifies the cluster to which the IP block belongs

In addition, with each IP address in the block, the following tags are added:

* `service-ip-<serviceID>=<ip>` - identifies the IP address assigned to the service

Note that the `<serviceID>` is hashed using sha256 and then base64-encoded, to prevent being able to identify the service from the tag.

When CCM encounters a `Service` of `type=LoadBalancer`, it will use the PhoenixNAP API to:

1. Look for a block of public IP addresses with fewer `service-ip-*` tags than IPs in the block. Else:
2. Request a new, location-specific IP block and tag it appropriately.
3. Select one IP from the block to assign to the Service.
3. Add a tag to the IP block assigning that IP to the specific service using `service-ip-<serviceID>`.

Finally, it will set the selected to `Service.Spec.LoadBalancerIP`.

#### Service Load Balancer IP Location
 
The CCM needs to determine where to request the IP block or find a block with available IPs.
It does not attempt to figure out where the nodes are, as that can change over time,
the nodes might not be in existence when the CCM is running or `Service` is created, and you could run a Kubernetes cluster across
multiple locations, or even cloud providers.

The CCM uses the following rules to determine where to create the IP:

1. if location is set globally using the environment variable `PNAP_LOCATION`, use it; else
1. if the `Service` for which the IP is being created has the annotation indicating the location, use it; else
1. Return an error, cannot use an IP from a block or create a block.

The overrides of environment variable and config file are provided so that you can control explicitly where the IPs
are created at a system-wide level, ignoring the annotations.

Using these flags and annotations, you can run the CCM on a node in a different location, or even outside of PhoenixNAP entirely.

#### Service LoadBalancer Implementations

Loadbalancing is enabled as follows.

1. If the environment variable `PNAP_LOAD_BALANCER` is set, read that. Else...
1. If the config file has a key named `loadbalancer`, read that. Else...
1. Load balancing is disabled.

The value of the loadbalancing configuration is `<type>:///<detail>` where:

* `<type>` is the named supported type, of one of those listed below
* `<detail>` is any additional detail needed to configure the implementation, details in the description below

For loadbalancing for Kubernetes `Service` of `type=LoadBalancer`, the following implementations are supported:

* [pnap-l2](#pnap-l2)

##### pnap-l2

When the `pnap-l2` option is enabled, for user-deployed Kubernetes `Service` of `type=LoadBalancer`,
the PhoenixNAP CCM assigns an IP from a block for each such `Service`. If necessary, it first creates the block.

To enable it, set the configuration `PNAP_LOAD_BALANCER` or config `loadbalancer` to:

```
pnap-l2://
```

If `pnap-l2` management is enabled, then CCM does the following.

1. For each node currently in the cluster or added:
   * retrieve the node's PhoenixNAP ID via the node provider ID
   * add the information to appropriate annotations on the node
1. For each service of `type=LoadBalancer` currently in the cluster or added:
   * if an IP block with the appropriate tags exists, and the `Service` already has that IP address affiliated with it, it is ready; ignore
   * if an IP block with the appropriate tags exists, and the `Service` does not have that IP affiliated with it, add it to the [service spec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#servicespec-v1-core)
   * if an IP block with the appropriate tags does not exist, but an IP block with unused IPs exists, allocate an IP and add it to the services spec
   * if an IP block with the appropriate tags does not exist, and no IP block with unused IPs exists, create a new IP block, allocate an IP and add it to the services spec 
1. For each service of `type=LoadBalancer` deleted from the cluster:
   * find the IP address from the service spec and remove it
   * remove the Service-specific tag from the IP block
   * if no other services are using the IP block, delete the IP block

## Core Control Loop

On startup, the CCM sets up the following control loop structures:

1. Implement the [cloud-provider interface](https://pkg.go.dev/k8s.io/cloud-provider#Interface), providing primarily the following API calls:
   * `Initialize()`
   * `InstancesV2()`
   * `LoadBalancer()`

## IP Configuration

If a loadbalancer is enabled, CCM creates a PhoenixNAP IP block and reserves an IP in the block for each `Service` of
`type=LoadBalancer`. It tags the Reservation with the following tags:

* `usage="cloud-provider-pnap-auto"`
* `service="<service-hash>"` where `<service-hash>` is the sha256 hash of `<namespace>/<service-name>`. We do this so that the name of the service does not leak out to PhoenixNAP itself.
* `cluster=<clusterID>` where `<clusterID>` is the UID of the immutable `kube-system` namespace. We do this so that if someone runs two clusters in the same account, and there is one `Service` in each cluster with the same namespace and name, then the two IPs will not conflict.

## Running Locally

You can run the CCM locally on your laptop or VM, i.e. not in the cluster. This _dramatically_ speeds up development. To do so:

1. Deploy everything except for the `Deployment` and, optionally, the `Secret`
1. Build it for your local platform `make build`
1. Set the environment variable `CCM_SECRET` to a file with the secret contents as a json, i.e. the content of the secret's `stringData`, e.g. `CCM_SECRET=ccm-secret.yaml`
1. Set the environment variable `KUBECONFIG` to a kubeconfig file with sufficient access to the cluster, e.g. `KUBECONFIG=mykubeconfig`
1. Set the environment variable `PNAP_LOCATION` to the correct location where the cluster is running, e.g. `PNAP_LOCATION="SEA`
1. If you want to run a loadbalancer, and it is not yet deployed, deploy it appropriately.
1. Enable the loadbalancer by setting the environment variable `PNAP_LOAD_BALANCER=pnap-l2://`
1. Run the command.

There are multiple ways to run the command.

In all cases, for lots of extra debugging, add `--v=2` or even higher levels, e.g. `--v=5`.

### Docker

```
docker run --rm -e PNAP_LOCATION=${PNAP_LOCATION} -e PNAP_LOAD_BALANCER=${PNAP_LOAD_BALANCER} phoenixnap/cloud-provider-pnap:latest --cloud-provider=phoenixnap --leader-elect=false --authentication-skip-lookup=true --cloud-config=$CCM_SECRET --kubeconfig=$KUBECONFIG
```

### Go toolchain

```
PNAP_LOCATION=${PNAP_LOCATION} PNAP_LOAD_BALANCER=${PNAP_LOAD_BALANCER} go run . --cloud-provider=phoenixnap --leader-elect=false --authentication-skip-lookup=true --cloud-config=$CCM_SECRET --kubeconfig=$KUBECONFIG
```

### Locally compiled binary

```
PNAP_LOCATION=${PNAP_LOCATION} dist/bin/cloud-provider-pnap-darwin-amd64 --cloud-provider=phoenixnap --leader-elect=false --authentication-skip-lookup=true --cloud-config=$CCM_SECRET --kubeconfig=$KUBECONFIG
```
