# UID 0 Is a Lie (PoC)

> Root inside the pod is not necessarily root on the node.

This directory contains a minimal PoC for Kubernetes User Namespaces (`spec.hostUsers` to `false`). It demonstrates that a process reporting `uid=0(root)` inside the container is mapped to a high, unprivileged host UID (often starting at `65536`) and is therefore blocked from writing to host-owned paths after a filesystem escape or hostPath mount.

## Requirements

* Kubernetes v1.36+ (User Namespaces GA)
* Node kernel >= 6.3 (idmapped mounts for tmpfs/Secrets/ConfigMaps)
* `kubectl`
* Tetragon installed (for the eBPF reveal)

## Minikube lab (recommended)

This is the recommended way to run and prove the PoC end-to-end in a local lab. You can reproduce it in other environments, but the key requirement remains: the node kernel must support idmapped mounts (kernel >= 6.3).

1. Create a local Kubernetes v1.36 cluster:

```bash
minikube start --kubernetes-version=v1.36.0 --driver=kvm2 --container-runtime=containerd
kubectl version -o yaml | sed -n '1,120p'
minikube ssh -- uname -r
```

2. Install Tetragon:

```bash
helm repo add cilium https://helm.cilium.io
helm repo update
helm install tetragon cilium/tetragon -n kube-system
kubectl rollout status -n kube-system ds/tetragon -w
```

Enable host credential reporting (so `process_credentials.uid/euid` is present in events):

```bash
kubectl -n kube-system patch cm tetragon-config --type merge -p '{"data":{"enable-process-cred":"true"}}'
kubectl -n kube-system rollout restart ds/tetragon
kubectl -n kube-system rollout status ds/tetragon -w
```

3. Prepare the hostPath inside the minikube node:

```bash
minikube ssh -- 'sudo mkdir -p /var/lib/userns-demo && sudo chown root:root /var/lib/userns-demo && sudo chmod 700 /var/lib/userns-demo && sudo rm -f /var/lib/userns-demo/proof-from-container.txt'
```

## Run

> This proves the identity remap (host UID changes), not a full "escape then blocked write" scenario. On modern clusters, Kubernetes may use idmapped mounts for volumes so that a user-namespaced pod can still read/write mounted paths without recursive `chown`. In a real escape-to-host-filesystem scenario (outside an idmapped mount), the same mapped high host UID is what typically turns privileged post-escape writes into `EACCES` ("Permission denied").

1. Apply the Tetragon policy:

```bash
kubectl apply -f tetragon-tracingpolicy.yaml
```

2. Deploy the baseline pod (should succeed writing to the hostPath because host sees UID 0):

```bash
kubectl apply -f pod-baseline.yaml
kubectl logs -f pod/userns-demo-baseline
```

3. Deploy the hardened pod (expect `uid_map` to show a high mapped host UID; the write may or may not be denied depending on idmapped mounts):

```bash
kubectl apply -f pod-hardened.yaml
kubectl logs -f pod/userns-demo-hardened
```

4. Watch Tetragon events from inside the DaemonSet (recommended) and compare `process_credentials.uid` (host UID) for baseline vs hardened:

```bash
TETRA_POD=$(kubectl -n kube-system get pod -l app.kubernetes.io/name=tetragon -o jsonpath='{.items[0].metadata.name}')
kubectl -n kube-system exec -it "$TETRA_POD" -c tetragon -- \
  tetra getevents --server-address unix:///var/run/tetragon/tetragon.sock -o json \
  | jq -rc '
      select(.process_exec or .process_kprobe or .process_tracepoint)
      | .process_exec // .process_kprobe // .process_tracepoint
      | select((.process.pod.name // "") | test("^userns-demo-(baseline|hardened)$"))
      | {
          pod: .process.pod.name,
          binary: .process.binary,
          uid: (.process.process_credentials.uid // .process.uid),
          euid: (.process.process_credentials.euid // .process.uid)
        }'
```

`tetra getevents` streams live events (it does not print historical ones). Keep it running, then trigger activity (for example, in another terminal):

```bash
kubectl exec userns-demo-baseline -- sh -lc 'cat /host/proof-from-container.txt >/dev/null || true'
kubectl exec userns-demo-hardened -- sh -lc 'cat /host/proof-from-container.txt >/dev/null || true'
```

The hardened pod still reports `uid=0` inside the container, but Tetragon should show a high host UID (mapped identity) in `process_credentials.uid`. Note that writes via volumes may still succeed due to idmapped mounts; the key proof is the host UID seen by the kernel.

## Verify the mapping (inside the pod)

```bash
kubectl exec -it userns-demo-hardened -- sh -lc 'id; echo; cat /proc/self/uid_map'
```

You should see `uid=0(root)` from `id`, but `uid_map` will show UID 0 mapped to a high host UID range (commonly starting at `65536`).

## Verify the mapping (on the node)

Find the container process PID and show the host UID:

```bash
POD_UID=$(kubectl get pod userns-demo-hardened -o jsonpath='{.metadata.uid}')
sudo crictl ps -q --label io.kubernetes.pod.uid="${POD_UID}" | head -n 1
```

Then (runtime-specific) inspect the process list; the key idea is that the host sees the container process running as a high UID, not 0.

## Cleanup

```bash
kubectl delete -f tetragon-tracingpolicy.yaml --ignore-not-found
kubectl delete -f pod-hardened.yaml --ignore-not-found
kubectl delete -f pod-baseline.yaml --ignore-not-found
```
