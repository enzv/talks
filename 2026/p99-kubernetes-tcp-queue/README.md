# TCP notsent PoC

This PoC asks one question:

> Can bulk traffic on one multiplexed HTTP/2/TCP connection inflate tail
> latency, and can `tcp_notsent_lowat` reduce it without hurting throughput?

The answer is measured, not guessed.

The server sends large `/bulk` responses while the client sends small `/ping`
requests through the same HTTP/2 connection. The eBPF collector measures:

```text
tcp_notsent_bytes = write_seq - snd_nxt
```

The experiment compares three server Pods:

```text
baseline       tcp_notsent_lowat = UINT_MAX
notsent-128k   tcp_notsent_lowat = 131072
notsent-4k     tcp_notsent_lowat = 4096
```

The HTTP/2 scheduler is the same in every variant: `small_first`. The only
treatment variable is the Pod sysctl.

## Run it

Requirements:

```text
go, podman, minikube, kubectl, helm, jq
```

Run the whole thing:

```sh
make
```

`make` does the boring parts for you:

1. runs the Go tests;
2. deletes and recreates only the `p99-tcp-queue` Minikube profile;
3. starts Kubernetes `v1.37.0`, where the PoC sysctl is safe and enabled by default;
4. builds and loads the container image;
5. applies `kubernetes.yml`;
6. installs Prometheus with Helm;
7. runs the real Kubernetes E2E;
8. writes the result to `result.json`.

Why was there an ugly kubelet flag in older versions? Before Kubernetes
`v1.37`,
`net.ipv4.tcp_notsent_lowat` is not in the kubelet safe-sysctl allowlist, so
Minikube must explicitly allow it:

```text
--extra-config=kubelet.allowed-unsafe-sysctls=net.ipv4.tcp_notsent_lowat
```

Kubernetes `v1.37` adds this namespaced sysctl to the safe list on Linux
kernels `4.6+`. The workaround is then unnecessary, which is why this PoC now
uses `v1.37.0` directly.

The Docker driver is intentional. The eBPF collector needs usable kernel BTF,
and this setup keeps the collector on the Minikube Docker kernel.

`make` is fresh by design. It does not reuse old PoC workloads or Prometheus
data. It also does not delete your other Minikube profiles.

The last command prints the compact result. The complete result is in:

```sh
jq . result.json
```

Run only local tests with:

```sh
make test
```

## What the result means

`result.json` contains one object per variant with:

- effective lowat and exact scenario socket identity;
- isolated and concurrent P99 latency;
- kernel-side `max_notsent`;
- sampled current notsent series for the scenario window;
- bulk throughput and request errors;
- qdisc samples, backlog, drops, and retransmission evidence;
- Kubernetes, kernel, and Go versions.

The two notsent metrics are deliberately different:

```text
tcpqueue_tcp_notsent_bytes
    sampled current value for write_seq - snd_nxt

tcpqueue_tcp_notsent_bytes_max
    maximum retained inside the eBPF map
```

The sampled series can miss a short peak. Therefore `max_notsent` comes from
the kernel-maintained maximum, not from the graph.

The current validated shape is roughly:

```text
                 baseline       128k
scenario P99      344 ms         169 ms
max notsent       3.36 MiB       195 KiB
bulk throughput   11.82 MB/s     11.82 MB/s
```

That is the point of the PoC: less data committed in TCP, lower tail latency,
same bulk throughput.

## Prometheus

After `make` has installed Prometheus, run:

```sh
make prometheus-ui
```

Open <http://127.0.0.1:9090>.

Get the exact labels from `result.json`:

```sh
jq '.baseline | {cgroup,socket,lowat}' result.json
jq '."128k" | {cgroup,socket,lowat}' result.json
```

Query one exact scenario socket:

```promql
tcpqueue_tcp_notsent_bytes{
  cgroup_id="SCENARIO_CGROUP",
  socket_cookie="SCENARIO_SOCKET"
} / 1024 / 1024
```

Use the `window_ns` values in `result.json` to choose the concurrent scenario
window. Do not use the isolated latency phase, and do not use
`tcpqueue_tcp_notsent_bytes_max` as the time-series curve.

Prometheus is intentionally ephemeral. The cluster is recreated by `make`,
so old samples do not contaminate a new run.

Stop the PoC resources with:

```sh
make clean
```

That removes the Prometheus release, PoC resources, and the dedicated
Minikube profile.
