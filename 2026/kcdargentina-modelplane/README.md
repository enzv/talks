# Modelplane Local Placement Demo

Demo for **Stop Babysitting GPUs and Learn Crossplane**.

I declare the requirement. Modelplane chooses the cluster. Kubernetes chooses the node. DRA allocates the device.

No web app. No custom CLI. Just Minikube, `kubectl`, and real resources.

## Requirements

- Docker Engine
- Minikube `v1.39.0`
- `kubectl`
- Helm 3
- Git and `curl`
- Roughly 24 GiB (Minikube profiles use 20 GiB total)

> Pinned setup: Kubernetes v1.34.0, Crossplane v2.4.0, Modelplane v0.4.0, MetalLB v0.14.8

Use Minikube v1.39.0 with Kubernetes v1.34.0. Older Minikube versions fail during kubeadm setup.

The fake devices are local DRA metadata. No physical GPU, CUDA, NVIDIA driver, cloud account, or model server is required.

## Prepare the platform

Run this before the talk:

```bash
make up
```

`up` finishes with the same checks as `make ready`.

`make up` creates control-plane with Crossplane and Modelplane, worker-a with a simulated 80Gi device, and worker-b with a simulated 141Gi device.

It installs the local DRA example driver, builds the `worker-b` image with `141Gi`, registers both `InferenceCluster` resources, and applies `platform.yaml`.

It does **not** apply `conference-modeldeployment.yaml`.

If `172.28.0.0/16` is already used:

```bash
make up \
  NETWORK_SUBNET=172.30.0.0/16 \
  NODE_PREFIX=172.30.0 \
  LB_PREFIX=172.30
```

Other targets:

```bash
make help
make stop     # stop the profiles and release their CPU and RAM
make start    # start the existing profiles again
make reset    # remove the demo workload and derived objects
make down     # delete the Minikube profiles and Docker network
```

Use stop/start when you need to release RAM without deleting the clusters.

## Live demo

### 1. Show the fleet

```bash
kubectl --context control-plane get inferenceclusters \
  -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,DEVICE:.status.gpuPools[0].devices[0].attributes.model.string,MEMORY:.status.gpuPools[0].devices[0].capacity.memory.value'
```

Expected capacity: worker-a 80Gi, worker-b 141Gi.

### 2. Show the requirement

Open `conference-modeldeployment.yaml` and search for `100Gi`.

The workload asks for at least `100Gi`. It does not name a cluster.

### 3. Apply and watch placement

Pane 1:

```bash
kubectl --context control-plane -n ml-team get modelreplicas --watch -o wide
```

Pane 2:

```bash
kubectl --context control-plane apply -f conference-modeldeployment.yaml
```

Read the `CLUSTER` column. It should show worker-b.

The value comes from ModelReplica.spec.clusterName.

### 4. Show the Node

```bash
REPLICA="$(kubectl --context control-plane -n ml-team get modelreplica \
  -l modelplane.ai/deployment=conference-model \
  -o jsonpath='{.items[0].metadata.name}')"
CLUSTER="$(kubectl --context control-plane -n ml-team get modelreplica "$REPLICA" \
  -o jsonpath='{.spec.clusterName}')"

kubectl --context "$CLUSTER" -n default get pods \
  -l "modelplane.ai/serving=$REPLICA" \
  -o custom-columns='POD:.metadata.name,PHASE:.status.phase,NODE:.spec.nodeName'
```

The Node is read from `Pod.spec.nodeName`.

### 5. Show the DRA device

```bash
POD="$(kubectl --context "$CLUSTER" -n default get pods \
  -l "modelplane.ai/serving=$REPLICA" \
  -o jsonpath='{.items[0].metadata.name}')"
CLAIM="$(kubectl --context "$CLUSTER" -n default get pod "$POD" \
  -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')"

kubectl --context "$CLUSTER" -n default get resourceclaim "$CLAIM" \
  -o jsonpath='{range .status.allocation.devices.results[*]}device={.device}{" driver="}{.driver}{" pool="}{.pool}{"\n"}{end}'
```

The device comes from `ResourceClaim.status.allocation.devices.results[]`.

Finish with: Modelplane to Cluster, Kubernetes to Node, DRA to Device.

The Pod may still be `Pending` or `ContainerCreating`. This demo proves placement and allocation, not model readiness.

## Reset

```bash
make reset
make ready
```
