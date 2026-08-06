# Run the agent yourself and read every byte it sends

Reading the source tells you what the agent is supposed to do. This walkthrough
shows you what it actually does: it runs the agent on a throwaway cluster on
your own machine, puts a receiver you control in the place of both the Struct8
API and your Prometheus, and prints everything either of them gets.

The result worth watching is how little arrives. The agent sends one message
unasked — the address it can be reached at — and after that the terminal stays
quiet until you ask it something.

Nothing here involves Struct8. No account, no credential, no network call to
us. The whole thing is deleted in one command at the end.

## What you need

- Docker
- `kubectl`
- [`kind`](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) — a
  single binary that runs Kubernetes inside Docker
- Python 3, for the receiver

## 1. A throwaway cluster

```bash
kind create cluster --name struct8-agent-test --wait 90s
```

That is the whole prerequisite. Nothing has to be installed inside the cluster:
status comes from the Kubernetes API, and charts are forwarded to a Prometheus
that `receiver.py` will impersonate in step 3.

## 2. Build the image and load it into the cluster

```bash
docker build -t k8s-agent:dev ..
```

```bash
kind load docker-image k8s-agent:dev --name struct8-agent-test
```

`kind load` is required: the cluster runs in its own Docker container and
cannot see your machine's local images.

## 3. Start the receiver

In a separate terminal, from this directory:

```bash
python3 receiver.py
```

It listens on port 8099 and prints everything it receives, in full. It stands in
for both of the agent's counterparts at once — the Struct8 API and your
Prometheus — and now you own both.

## 4. Deploy the agent

```bash
kubectl --context kind-struct8-agent-test create namespace struct8-agent-test
```

```bash
kubectl --context kind-struct8-agent-test apply -f agent.yaml
```

[`agent.yaml`](agent.yaml) is self-contained and readable: ServiceAccount,
the read-only ClusterRole, its binding, and the Deployment. On Linux, change
`host.docker.internal` to your host's IP on the docker0 bridge.

**Now watch the receiver terminal, and notice that nothing happens.** No metric
is collected, batched or uploaded, because the agent no longer does any of
those. Leave it running as long as you like.

The agent's own log says the same thing from its side, starting with the version
it is built from:

```bash
kubectl --context kind-struct8-agent-test logs -n struct8-agent-test deployment/struct8-agent -f
```

The one message that does travel unasked is the announcement of the agent's own
address — and in this walkthrough it is not sent at all, because `agent.yaml`
deploys no tunnel and so there is no address to announce.

## 5. Prove the permissions are read-only

Kubernetes will answer this directly, which is stronger evidence than the
manifest:

```bash
SA=system:serviceaccount:struct8-agent-test:struct8-agent-sa
kubectl --context kind-struct8-agent-test auth can-i list pods --as=$SA -A
kubectl --context kind-struct8-agent-test auth can-i get secrets --as=$SA -A
kubectl --context kind-struct8-agent-test auth can-i delete pods --as=$SA -A
kubectl --context kind-struct8-agent-test auth can-i create deployments --as=$SA -A
```

Expected: `yes`, then `no`, `no`, `no`. The first is the agent's job; the rest
are permissions it was never granted.

## 6. The status endpoint

The status server binds to `127.0.0.1` inside the Pod and is not reachable
from anywhere else — there is no Service and no exposed port. `port-forward`
reaches the Pod's loopback interface:

```bash
kubectl --context kind-struct8-agent-test port-forward -n struct8-agent-test deployment/struct8-agent 18080:8080
```

Then, in another terminal:

```bash
curl -s -X POST http://127.0.0.1:18080/status -H "Content-Type: application/json" -d '{"resources":[{"kind":"Deployment","namespace":"kube-system","name":"coredns"},{"kind":"Deployment","namespace":"kube-system","name":"does-not-exist"},{"kind":"Invented","namespace":"kube-system","name":"x"}]}'
```

The answers come back in the order asked:

```json
[{"deployed":true,"status":"2/2","uid":"...","outputs":{"desiredReplicas":2,"readyReplicas":2}},
 {"deployed":false,"status":"","uid":"","outputs":{}},
 {"deployed":false,"status":"unknown_kind","uid":"","outputs":{}}]
```

The third result is the boundary: a kind outside the ClusterRole is refused by
the agent itself, before any API call is made.

In this walkthrough nothing outside the cluster can reach that endpoint —
`agent.yaml` deploys no tunnel container. In a real deployment, reaching it
requires a Cloudflare Tunnel token that you supply. Omit it and the status
channel does not exist.

## 7. A chart's query

With the same `port-forward` still running, ask for a chart the way Struct8 does:

```bash
curl -s -X POST http://127.0.0.1:18080/metrics-query -H "Content-Type: application/json" -d '{"metric":"cpu","namespace":"kube-system","kind":"Deployment","name":"coredns","start":1785975600,"end":1785979200,"granularity":"minute","promql":"sum(rate(container_cpu_usage_seconds_total{namespace=\"{namespace}\",pod=~\"{name}-.*\"}[5m]))"}'
```

The receiver terminal prints the query it received, and the agent returns the
series the receiver invented for it:

```json
{"bucketSeconds":60,"points":[{"bucket":1785975600,"value":0.25},{"bucket":1785975900,"value":0.5}]}
```

Two things are visible in that exchange. The query is **not** the agent's: it
arrived in the request, from the resource's entry in Struct8's catalogue, and
the agent refuses a request that carries none. And `{namespace}` and `{name}`
were filled in here, escaped, so a resource named `a"} or up{` cannot rewrite
the query into one about something else — see `renderPromQL` in
[`../query.go`](../query.go).

## 8. Delete everything

```bash
kind delete cluster --name struct8-agent-test
```

The cluster, the agent, and the RBAC go with it. Nothing was created outside
Docker.
