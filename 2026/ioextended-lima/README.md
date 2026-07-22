# Agent Workload Lab

> Same image, different workload posture.

This PoC uses a tiny Go HTTP service that mimics an agent-like command runner. It is not a real agent. It maps prompt text to deterministic shell commands so the pod posture is easy to inspect.

The walkthrough follows `agent-baseline` first. Then it compares that pod with `agent-hardened` and `agent-sandbox-hardened`.

The point is precise: Agent Sandbox changes the API and lifecycle object. The hardening still comes from the pod template: `runtimeClassName`, service account token policy, UID, seccomp, Linux capabilities, read-only root filesystem, volumes, and network policy.

## Requirements

* `kubectl`
* `minikube`
* `docker`
* `helm`
* `git`
* Linux host with Minikube `kvm2`
* Kubernetes `v1.36.0` or similar

## Environment

```bash
PROFILE=agent-workload-lab
NS=agent-lab
IMAGE=agent-workload-lab:latest
```

## 1. Create The Cluster

```bash
minikube start -p "agent-workload-lab" \
  --driver=kvm2 \
  --nodes=1 \
  --cpus=2 \
  --memory=4096 \
  --container-runtime=containerd \
  --kubernetes-version=v1.36.0
```

```bash
kubectl config use-context "$PROFILE"
kubectl get nodes -o wide
minikube -p "$PROFILE" status
```

```bash
kubectl get node "$PROFILE" -o jsonpath='{.status.nodeInfo.containerRuntimeVersion}{"\n"}'
minikube -p "$PROFILE" ssh -- "sudo crictl info | head -40"
```

## 2. Confirm VM Internet

The Minikube VM must reach the Internet. The host reaching the Internet is not enough.

```bash
curl -I --connect-timeout 10 https://gcr.io/v2/
minikube -p "$PROFILE" ssh -- "curl -I --connect-timeout 10 https://gcr.io/v2/"
minikube -p "$PROFILE" ssh -- "ping -c 3 8.8.8.8"
```

`https://gcr.io/v2/` should return `401 Unauthorized`. That is good: the registry is reachable.

If the host works but the VM times out, inspect libvirt NAT and host forwarding:

```bash
virsh -c qemu:///system net-list --all
virsh -c qemu:///system domiflist "$PROFILE"
sysctl net.ipv4.ip_forward
sudo -n iptables -S FORWARD
sudo -n iptables -t nat -S
```

## 3. Enable gVisor

Kubernetes needs a `RuntimeClass`. The node also needs `runsc` registered in `containerd`.

Try the Minikube addon first:

```bash
minikube -p "$PROFILE" addons enable gvisor
kubectl get runtimeclass
```

If the addon fails, install gVisor manually inside the Minikube VM:

```bash
minikube -p "$PROFILE" ssh -- "curl -fsSLo /tmp/runsc https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/runsc"
minikube -p "$PROFILE" ssh -- "curl -fsSLo /tmp/runsc.sha512 https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/runsc.sha512"
minikube -p "$PROFILE" ssh -- "curl -fsSLo /tmp/containerd-shim-runsc-v1 https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/containerd-shim-runsc-v1"
minikube -p "$PROFILE" ssh -- "curl -fsSLo /tmp/containerd-shim-runsc-v1.sha512 https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/containerd-shim-runsc-v1.sha512"
```

```bash
minikube -p "$PROFILE" ssh -- "cd /tmp && sha512sum -c runsc.sha512"
minikube -p "$PROFILE" ssh -- "cd /tmp && sha512sum -c containerd-shim-runsc-v1.sha512"
```

```bash
minikube -p "$PROFILE" ssh -- "sudo install -m 0755 /tmp/runsc /usr/bin/runsc"
minikube -p "$PROFILE" ssh -- "sudo install -m 0755 /tmp/containerd-shim-runsc-v1 /usr/bin/containerd-shim-runsc-v1"
minikube -p "$PROFILE" ssh -- "runsc --version"
```

Append the `runsc` runtime to `containerd` if it is not already present:

```bash
minikube -p "$PROFILE" ssh -- "grep -q 'containerd.runtimes.runsc' /etc/containerd/config.toml"
```

Only if the previous command exits non-zero:

```bash
cat >/tmp/runsc-containerd.toml <<'EOF2'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF2
```

```bash
cat /tmp/runsc-containerd.toml | minikube -p "$PROFILE" ssh -- "sudo tee -a /etc/containerd/config.toml >/dev/null"
minikube -p "$PROFILE" ssh -- "sudo systemctl restart containerd kubelet"
```

Create the `RuntimeClass` if needed:

```bash
cat >/tmp/runtimeclass-gvisor.yaml <<'EOF2'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF2
```

```bash
kubectl apply -f /tmp/runtimeclass-gvisor.yaml
kubectl get runtimeclass gvisor -o yaml
```

Verify the node runtime config:

```bash
minikube -p "$PROFILE" ssh -- "grep -n 'containerd.runtimes.runsc\|io.containerd.runsc.v1' /etc/containerd/config.toml"
```

## 4. Smoke Test gVisor

```bash
kubectl delete pod gvisor-smoke --ignore-not-found
```

```bash
kubectl run gvisor-smoke \
  --image=busybox:1.36 \
  --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"gvisor","containers":[{"name":"gvisor-smoke","image":"busybox:1.36","command":["sh","-c","echo runtime-ok && sleep 3600"]}]}}'
```

```bash
kubectl wait --for=condition=Ready pod/gvisor-smoke --timeout=120s
kubectl logs gvisor-smoke
kubectl describe pod gvisor-smoke | grep -E 'Runtime Class Name|Status|Container ID'
```

```bash
POD_ID=$(minikube -p "$PROFILE" ssh -- "sudo crictl pods --name gvisor-smoke -q")
minikube -p "$PROFILE" ssh -- "sudo crictl inspectp $POD_ID | grep -i -C 2 runtime"
```

Expected:

```text
runtimeHandler: runsc
runtimeType: io.containerd.runsc.v1
```

```bash
kubectl delete pod gvisor-smoke --wait=true
```

## 5. Build And Load The Image

```bash
docker build --provenance=false -f Containerfile -t "$IMAGE" .
minikube -p "$PROFILE" image load "$IMAGE"
minikube -p "$PROFILE" ssh -- "sudo crictl images | grep agent-workload-lab"
```

## 6. Read The Baseline Manifest

```bash
sed -n '1,45p' agent-workload-lab.yaml
```

Baseline is intentionally plain:

* no `runtimeClassName`
* no `securityContext`
* default service account token is mounted
* process runs as root
* root filesystem is writable
* `/workspace` is writable because it is an explicit `emptyDir`

## 7. Run `agent-baseline`

```bash
kubectl apply -f agent-workload-lab.yaml
kubectl -n "$NS" wait --for=condition=Ready pod/agent-baseline --timeout=120s
```

```bash
kubectl -n "$NS" get pod agent-baseline -o wide
kubectl -n "$NS" describe pod agent-baseline | grep -E 'Runtime Class Name|Service Account|Mounts|Container ID'
```

```bash
POD=agent-baseline
POD_ID=$(minikube -p "$PROFILE" ssh -- "sudo crictl pods --name $POD -q")
minikube -p "$PROFILE" ssh -- "sudo crictl inspectp $POD_ID | grep -i -C 2 runtime"
```

Expected runtime:

```text
runtimeType: io.containerd.runc.v2
```

## 8. Exercise `agent-baseline`

```bash
POD=agent-baseline
PROMPT=identity
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: `uid=0(root)`.

```bash
PROMPT=token
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: succeeds, because the default service account token is mounted.

```bash
PROMPT=rootfs
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: succeeds, because the image root filesystem is writable.

```bash
PROMPT=workspace
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: succeeds, because `/workspace` is an explicit writable `emptyDir`.

## 9. Compare With `agent-hardened`

```bash
sed -n '46,96p' agent-workload-lab.yaml
```

`agent-hardened` changes the pod posture:

* `runtimeClassName: gvisor`
* `automountServiceAccountToken: false`
* `runAsUser: 10001`
* `seccompProfile: RuntimeDefault`
* `allowPrivilegeEscalation: false`
* `capabilities.drop: [ALL]`
* `readOnlyRootFilesystem: true`
* writable `emptyDir` only at `/workspace` and `/tmp`

```bash
kubectl -n "$NS" wait --for=condition=Ready pod/agent-hardened --timeout=120s
kubectl -n "$NS" get pod agent-hardened -o wide
kubectl -n "$NS" describe pod agent-hardened | grep -E 'Runtime Class Name|SeccompProfile|Service Account|Mounts|Container ID'
```

```bash
POD=agent-hardened
POD_ID=$(minikube -p "$PROFILE" ssh -- "sudo crictl pods --name $POD -q")
minikube -p "$PROFILE" ssh -- "sudo crictl inspectp $POD_ID | grep -i -C 2 runtime"
```

Expected runtime:

```text
runtimeHandler: runsc
runtimeType: io.containerd.runsc.v1
```

```bash
POD=agent-hardened
PROMPT=identity
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: `uid=10001`.

```bash
PROMPT=token
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: fails, because the service account token is not mounted.

```bash
PROMPT=rootfs
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: fails, because the image root filesystem is read-only.

```bash
PROMPT=workspace
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: succeeds. Hardened workloads still need an explicit scratchpad.

## 10. Install Agent Sandbox

```bash
git clone https://github.com/kubernetes-sigs/agent-sandbox /tmp/agent-sandbox
```

```bash
helm install agent-sandbox /tmp/agent-sandbox/helm \
  --namespace agent-sandbox-system \
  --create-namespace \
  --set image.tag=v0.4.6 \
  --set controller.extensions=true
```

```bash
kubectl api-resources | grep -i sandbox
kubectl -n agent-sandbox-system get pods
kubectl -n agent-sandbox-system logs deploy/agent-sandbox-controller --tail=80
```

## 11. Run `agent-sandbox-hardened`

```bash
sed -n '1,120p' sandbox-hardened.yaml
```

```bash
kubectl apply -f sandbox-hardened.yaml
kubectl -n "$NS" wait --for=condition=Ready pod/agent-sandbox-hardened --timeout=120s
```

```bash
kubectl -n "$NS" get sandbox agent-sandbox-hardened
kubectl -n "$NS" get pod agent-sandbox-hardened -o wide
kubectl -n "$NS" describe pod agent-sandbox-hardened | grep -E 'Controlled By|Runtime Class Name|SeccompProfile|Service Account|Mounts|Container ID'
```

```bash
POD=agent-sandbox-hardened
POD_ID=$(minikube -p "$PROFILE" ssh -- "sudo crictl pods --name $POD -q")
minikube -p "$PROFILE" ssh -- "sudo crictl inspectp $POD_ID | grep -i -C 2 runtime"
```

Expected runtime:

```text
runtimeHandler: runsc
runtimeType: io.containerd.runsc.v1
```

## 12. Diff Direct Hardened vs Sandbox Hardened

Save the two realized Pod specs:

```bash
kubectl -n "$NS" get pod agent-hardened -o yaml > /tmp/agent-hardened.pod.yaml
kubectl -n "$NS" get pod agent-sandbox-hardened -o yaml > /tmp/agent-sandbox-hardened.pod.yaml
```

Diff them:

```bash
diff -u /tmp/agent-hardened.pod.yaml /tmp/agent-sandbox-hardened.pod.yaml | sed -n '1,220p'
```

The useful differences are lifecycle and ownership details:

* `agent-sandbox-hardened` is controlled by `Sandbox/agent-sandbox-hardened`
* the controller adds Sandbox labels and annotations
* the hardened pod template fields remain the same security boundary
* both hardened pods use `runtimeClassName: gvisor`

This is the point: the new API changes how the workload is requested and owned. It does not invent the hardening fields.

## 13. Exercise `agent-sandbox-hardened`

```bash
POD=agent-sandbox-hardened
PROMPT=identity
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: `uid=10001`.

```bash
PROMPT=token
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: fails, because the service account token is not mounted.

```bash
PROMPT=rootfs
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: fails, because the image root filesystem is read-only.

```bash
PROMPT=workspace
kubectl -n "$NS" exec "$POD" -- wget -qO- --header 'Content-Type: application/json' --post-data "{\"prompt\":\"$PROMPT\"}" http://127.0.0.1:8080/run
```

Expected: succeeds, because `/workspace` is an explicit writable `emptyDir`.

## Cleanup

```bash
kubectl delete -f sandbox-hardened.yaml --ignore-not-found
kubectl delete -f agent-workload-lab.yaml --ignore-not-found
```

```bash
helm uninstall agent-sandbox -n agent-sandbox-system --ignore-not-found
kubectl delete namespace agent-sandbox-system --ignore-not-found
```

```bash
minikube delete -p "$PROFILE"
```
