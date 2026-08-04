# Run the agent yourself and read what it sends

Reading the source tells you what the agent is supposed to do. This walkthrough
shows you what it actually does: it runs the agent on a throwaway cluster on
your own machine, points it at a receiver you control instead of the Struct8
API, and prints every byte that would have left your network.

Nothing here involves Struct8. No account, no credential, no network call to
us. The whole thing is deleted in one command at the end.

## What you need

- Docker
- `kubectl`
- [`kind`](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) — a
  single binary that runs Kubernetes inside Docker
- Python 3, for the receiver

## 1. A throwaway cluster with metrics-server

```bash
kind create cluster --name struct8-agent-test --wait 90s
```

```bash
kubectl --context kind-struct8-agent-test apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
```

kind nodes use self-signed kubelet certificates, which metrics-server rejects
by default — without the following patch it stays NotReady forever and no
metrics ever appear:

```bash
kubectl --context kind-struct8-agent-test patch deployment metrics-server -n kube-system --type=json -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

```bash
kubectl --context kind-struct8-agent-test top nodes
```

When that last command returns numbers, the data source the agent depends on
is working.

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

It listens on port 8099 and prints every batch it receives, in full. This is
the Struct8 API's place in the chain, and now you own it.

## 4. Deploy the agent

```bash
kubectl --context kind-struct8-agent-test create namespace struct8-agent-test
```

```bash
kubectl --context kind-struct8-agent-test apply -f agent.yaml
```

[`agent.yaml`](agent.yaml) is self-contained and readable: ServiceAccount,
the read-only ClusterRole, its binding, and the Deployment. On Linux, change
`WORKER_BASE_URL` from `host.docker.internal` to your host's IP on the docker0
bridge.

Within about ten seconds the receiver terminal starts printing batches:

```json
{
  "points": [
    {"metric": "cpu", "namespace": "kube-system", "kind": "Deployment",
     "name": "coredns", "value": 0.004},
    {"metric": "memory", "namespace": "kube-system", "kind": "Deployment",
     "name": "coredns", "value": 14.2}
  ]
}
```

**That is the entire payload.** Five fields per point — `metric`, `namespace`,
`kind`, `name`, `value` — where `cpu` is in cores and `memory` in MiB. The
struct that produces it is `metricPoint` in [`../metrics.go`](../metrics.go),
and it has no other fields. No object contents, no environment variables, no
logs. Leave it running as long as you like; the shape never changes.

The agent's own logs show the same cycle from its side:

```bash
kubectl --context kind-struct8-agent-test logs -n struct8-agent-test deployment/struct8-agent -f
```

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

## 7. Delete everything

```bash
kind delete cluster --name struct8-agent-test
```

The cluster, the agent, and the RBAC go with it. Nothing was created outside
Docker.
