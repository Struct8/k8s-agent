# Struct8 Kubernetes Agent

Reports CPU and memory usage per workload, and the deployment status of
individual objects, from a Kubernetes cluster back to [Struct8](https://struct8.com).
It is the data source behind the metric charts and the deployed/not-deployed
state shown on a Struct8 diagram.

It runs as a single Deployment in your cluster. There is no Helm chart to
install by hand: Struct8 generates the manifest, and it reaches the cluster
through whatever GitOps flow already deploys the rest of your workloads.

This repository exists so you can read exactly what runs inside your cluster
before you agree to run it. That is the point of it being public.

## What it reads, and what it cannot do

The agent holds a **read-only** ClusterRole. It has no `create`, `update`,
`patch`, or `delete` verb on anything, so it cannot change the state of your
cluster even if it were compromised.

It reads two things:

- **`metrics.k8s.io`** — CPU and memory of running Pods. This comes from
  metrics-server, the same source as `kubectl top`.
- **Object status** — the `.status` of the object kinds listed in the RBAC
  below, to answer whether a given object exists and is healthy.

It does **not** read Secrets, ConfigMaps, environment variables, container
logs, or the contents of any volume. It does not execute into containers.
Those permissions are absent from the ClusterRole, not merely unused.

### The exact ClusterRole

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods", "services", "endpoints", "nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["pods", "nodes"]
    verbs: ["get", "list"]
  - apiGroups: ["apps"]
    resources: ["deployments", "daemonsets", "statefulsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
```

A status query for a kind outside this list returns `unknown_kind`. The agent
never attempts a call it lacks permission for.

## How data leaves the cluster

Two independent channels, with different network requirements. Metrics work
on their own; status is optional and can be left off entirely.

**Metrics — outbound push, no inbound access.** Every `PUSH_INTERVAL_SECONDS`
(20 by default) the agent collects usage and sends it over ordinary HTTPS to
the Struct8 API. Nothing outside the cluster connects in. If your egress
policy allows HTTPS to the configured endpoint, this works; there is no port
to open and no firewall rule to add.

**Status — pull, and only if you enable it.** Answering "does this object
exist right now" requires Struct8 to ask at the moment you look at the
diagram. The status server binds to `127.0.0.1:8080` and is **never** exposed
outside the Pod. Reaching it uses a second container in the same Pod running
[`cloudflared`](https://github.com/cloudflare/cloudflared), which opens an
outbound tunnel and forwards requests over the Pod's loopback interface. Leave
`cloudflare_tunnel_token` empty in Struct8 and that container is not deployed
at all — you get a metrics-only agent with no inbound path of any kind.

## Metrics are attributed per workload, not per Pod

A chart in Struct8 asks about `Deployment#checkout`, not about the individual
Pods behind it. So the agent resolves each Pod's owner through
`ownerReferences` (`Pod → ReplicaSet → Deployment`, or a single hop for
`StatefulSet`, `DaemonSet`, and `Job`) and sums the Pods before sending. A Pod
with no owner is reported as itself. See [`owners.go`](owners.go).

The chain deliberately stops at `Job` rather than climbing to `CronJob`: the
extra hop costs another listing every cycle for a kind nothing queries.

One visible consequence: during a rollout, Pods from the previous ReplicaSet
are still running and still counted, so the value rises until the rollout
finishes. That is the correct answer to "what does this workload consume right
now", not an artifact.

## Requirements

**metrics-server must already be installed.** The agent reads `metrics.k8s.io`
and does not provide it. Without metrics-server the agent still starts and
still answers status, but CPU and memory come back empty. Check with:

```bash
kubectl top nodes
```

If that command works, the agent has what it needs.

## Configuration

All configuration comes from environment variables. Nothing is baked into the
image — the same container serves any cluster, distinguished only by the
credentials injected at deploy time. Struct8 generates these for you; they are
documented here so you can verify the generated manifest.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `CLUSTER_ID` | yes | — | Identifies which cluster this agent reports for. |
| `CLUSTER_API_KEY` | yes | — | Write-only credential for pushing metrics. Delivered through a Kubernetes Secret, never in plain text in the Deployment. |
| `WORKER_BASE_URL` | yes | — | The Struct8 API endpoint that receives metrics. |
| `PUSH_INTERVAL_SECONDS` | no | `20` | Collection and send interval. Minimum 5. |
| `LISTEN_ADDR` | no | `127.0.0.1:8080` | Address of the local status server. |

`CLUSTER_API_KEY` grants **write only**. It can push metric points for its own
cluster and nothing else — it cannot read any data back, including your own.

## Images

```
ghcr.io/struct8/k8s-agent:<version>
```

Published for `linux/amd64` and `linux/arm64`. Built from
`gcr.io/distroless/static-debian12`, which contains no shell and no package
manager, and runs as UID 65532 rather than root.

Pin a version. The `latest` tag moves only on a tagged release, but with
`imagePullPolicy: IfNotPresent` a node can still serve a cached layer, which
makes an old build look like a sync that never happened.

## Verifying it yourself

[`local-test/`](local-test/) walks through running the agent against a
throwaway `kind` cluster on your own machine: create the cluster, install
metrics-server, deploy the agent, and watch what it sends. Pointing it at a
local HTTP receiver instead of the Struct8 API shows you the exact payload
that would leave your network.

Building from source:

```bash
docker build -t k8s-agent:dev .
```

The build is fully static with `CGO_ENABLED=0`, so the binary in the published
image depends on nothing from the base image.

## License

Apache-2.0 — see [LICENSE](LICENSE).
