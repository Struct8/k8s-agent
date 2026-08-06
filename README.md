# Struct8 Kubernetes Agent

Answers two questions about a Kubernetes cluster on behalf of
[Struct8](https://struct8.com): the deployment status of individual objects, and
whatever your own Prometheus knows about them. It is what puts the
deployed/not-deployed state and the metric charts on a Struct8 diagram.

It stores nothing. Status is read from the Kubernetes API when you look at the
diagram, and a chart is forwarded to Prometheus and forwarded back — no series
is kept here, and none is sent anywhere on a timer.

It runs as a single Deployment in your cluster. There is no Helm chart to
install by hand: Struct8 generates the manifest, and it reaches the cluster
through whatever GitOps flow already deploys the rest of your workloads.

This repository exists so you can read exactly what runs inside your cluster
before you agree to run it. That is the point of it being public.

## What it reads, and what it cannot do

The agent holds a **read-only** ClusterRole. It has no `create`, `update`,
`patch`, or `delete` verb on anything, so it cannot change the state of your
cluster even if it were compromised.

It reads one thing from Kubernetes: the `.status` of the object kinds granted in
the ClusterRole, to answer whether a given object exists and is healthy.
Everything on a chart comes from Prometheus instead, over HTTP, using no
Kubernetes permission at all.

It does **not** read Secrets, ConfigMaps, environment variables, container
logs, or the contents of any volume. It does not execute into containers.
Those permissions are absent from the ClusterRole, not merely unused.

### The ClusterRole

Struct8 generates it from the resource types actually drawn on your diagram, so
the exact list is yours rather than fixed here. Its shape is always the same:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods", "services", "endpoints", "nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "daemonsets", "statefulsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
  # plus one entry per custom resource on the diagram -- Gateway API, Argo CD,
  # Kong and so on -- always with these same three read verbs.
```

Only read verbs appear, whatever is on the diagram. A status query for a kind
outside the list returns `unknown_kind`; the agent never attempts a call it
lacks permission for.

## How data leaves the cluster

**Nothing leaves on a timer.** The agent answers questions and is otherwise
silent. Data crosses the boundary only while someone has the diagram open, and
only about the resources drawn on it.

**Both channels are pull.** Answering "does this object exist right now", or
"what did CPU do last hour", requires Struct8 to ask at the moment you look. The
server binds to `127.0.0.1:8080` and is **never** exposed outside the Pod.
Reaching it uses a second container in the same Pod running
[`cloudflared`](https://github.com/cloudflare/cloudflared), which opens an
outbound tunnel and forwards requests over the Pod's loopback interface. There
is no inbound port to open and no firewall rule to add.

**The one outbound message.** Once the tunnel is up, the agent tells the Struct8
API which address to reach it at. That is the only thing it sends unasked, and it
carries no cluster data — just the endpoint and its token.

Leave `cloudflare_tunnel_token` empty in Struct8 and the tunnel container is not
deployed at all. The agent then has no inbound path of any kind, and neither
status nor charts can be answered.

## Metrics come from your Prometheus

The agent runs no collection of its own. A chart in Struct8 carries the query
that draws it — declared with the resource type, in Struct8's own catalogue —
and the agent forwards it to the Prometheus at `PROMETHEUS_URL`, then returns
the series.

Two consequences worth knowing:

- **Retention, resolution and cardinality are your Prometheus's business.** The
  agent holds no history, so the chart can go back as far as your retention does,
  and the Pod's memory does not grow with the number of workloads.
- **A chart can only show what Prometheus scrapes.** For CPU and memory per
  workload that means cAdvisor (through the kubelet) plus `kube-state-metrics`,
  which is what maps a Pod back to the Deployment that owns it. Without the
  second one, a chart asking about a Deployment answers empty.

## Requirements

**A reachable Prometheus.** The agent needs an HTTP address it can reach from
inside the cluster; the Prometheus Operator publishes one as
`http://prometheus-operated.<namespace>:9090`. Anything speaking the Prometheus
HTTP API works, including a managed one, as long as the Pod can reach it.

Leave `PROMETHEUS_URL` empty and the agent still starts and still answers status
— charts report that no Prometheus is configured, which is a different statement
from a workload that used nothing.

## Configuration

All configuration comes from environment variables. Nothing is baked into the
image — the same container serves any cluster, distinguished only by the
credentials injected at deploy time. Struct8 generates these for you; they are
documented here so you can verify the generated manifest.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `CLUSTER_ID` | yes | — | Identifies which cluster this agent answers for. |
| `CLUSTER_API_KEY` | yes | — | Credential for announcing this agent's address. Delivered through a Kubernetes Secret, never in plain text in the Deployment. |
| `WORKER_BASE_URL` | yes | — | The Struct8 API endpoint the address is announced to. |
| `PROMETHEUS_URL` | no | — | Where to send a chart's query, e.g. `http://prometheus-operated.monitoring:9090`. Empty means charts answer that no Prometheus is configured; status is unaffected. |
| `LISTEN_ADDR` | no | `127.0.0.1:8080` | Address of the local server. |
| `AUTH_TOKEN` | see note | — | Bearer token required on `/status` and `/metrics-query`. Mandatory whenever `LISTEN_ADDR` is not on the loopback — the agent refuses to start otherwise, rather than coming up healthy while exposing cluster state. |

`CLUSTER_API_KEY` grants **write only**. It can announce an address for its own
cluster and nothing else — it cannot read any data back, including your own.

`PUSH_INTERVAL_SECONDS`, `RETENTION_HOURS` and `MAX_SERIES` were removed in
0.4.0, when collection and the in-memory store were dropped. Leaving them in an
old manifest is harmless: they are ignored, deliberately, so that removing them
is not a condition for the agent to start.

## Images

```
ghcr.io/struct8/k8s-agent:<version>
```

Published for `linux/amd64` and `linux/arm64`. Built from
`gcr.io/distroless/static-debian12`, which contains no shell and no package
manager, and runs as UID 65532 rather than root.

Every push publishes more than one tag, and they do not behave alike:

| Tag | Moves? | What it is for |
| --- | --- | --- |
| `X.Y.Z` | never | A release. Deploy this. |
| `main-<sha>` | never | The build of one commit on `main`. Deploy this to try a change before it is released. |
| `X.Y`, `X` | on each release in that line | Reading a changelog, not a deployment. |
| `edge` | on every push to `main` | Reading, not a deployment. |
| `latest` | on each release | Reading, not a deployment. |

Deploy only a tag that never moves. A moving tag leaves the manifest byte for
byte identical from one build to the next, so a GitOps controller finds nothing
to apply, the Pod is never replaced, and the cluster keeps running the previous
build while every sync is reported as successful.

### Which build is running

The first line the agent writes to its log is its own version:

```
[agent] version 0.3.0
```

The same string comes back from `/healthz`, as `{"version":"0.3.0"}`.

This is the reading that settles a disagreement between the diagram and the
cluster, and it is the only one that can: the image reference in the manifest
says which build was requested, and a moving tag, a cached layer, or a Pod that
was never replaced each leave the two pointing at different code with nothing
reporting an error.

In Struct8, the same answer is on the agent node itself — the status panel shows
the `image` the running workload reports back.

## Verifying it yourself

[`local-test/`](local-test/) walks through running the agent against a
throwaway `kind` cluster on your own machine: create the cluster, deploy the
agent, and watch what it sends. A local HTTP receiver takes the place of both
the Struct8 API and your Prometheus, printing everything either one gets — so
what you end up reading is how little that is, and exactly what a status answer
and a chart query look like on the wire.

Building from source:

```bash
docker build -t k8s-agent:dev .
```

The build is fully static with `CGO_ENABLED=0`, so the binary in the published
image depends on nothing from the base image.

## License

Apache-2.0 — see [LICENSE](LICENSE).
