# sysctl-operator

A Kubernetes operator that applies and continuously verifies **kernel `sysctl`
parameters** on selected nodes, declaratively, via a `SysctlProfile` custom
resource.

## Description

You declare the sysctls you want and which nodes they belong on; the operator
does the rest:

1. **Render & apply** — it renders the profile's sysctls into a `ConfigMap` and
   rolls out an **applier DaemonSet** (pinned to your `nodeSelector`) whose pods
   run `sysctl -w` on the host to set the values.
2. **Report & teardown** — each worker pod reports its result to the operator's
   HTTP API. Once every selected node has reported success at the current config
   hash, the operator **tears the DaemonSet down** (the appliers have done their
   job) and records `teardownHash`.
3. **Drift check** — on the `schedule.driftCheck` cron, the operator runs a
   **CronJob** whose pods inspect the live values and report drift back to the
   same API, so divergence is caught after teardown.

Config identity is tracked by a **hash** of the rendered sysctls: workers refuse
to act on a ConfigMap whose hash doesn't match what the operator expects, and a
changed hash automatically triggers a fresh rollout.

### Components

| Component | Binary / Image | Role |
|-----------|----------------|------|
| **Manager** | `cmd/main.go` → `Dockerfile` | Reconciles `SysctlProfile`, owns the ConfigMap/DaemonSet/CronJob, serves the report API + metrics |
| **Worker** | `cmd/worker/main.go` → `Dockerfile.worker` | Applies (`MODE=apply`) or audits (`MODE=check`) sysctls on a node and reports back |

## The `SysctlProfile` resource

```yaml
apiVersion: sysctl.k8s.io/v1alpha1
kind: SysctlProfile
metadata:
  name: aks-network-optimized
  namespace: kube-system
spec:
  nodeSelector:                 # which nodes this profile targets
    matchLabels:
      sysctlstack: tuned

  rollout:
    batchSize: 10
    batchInterval: 30s
    failureThreshold: 2
    pauseOnFailure: true

  schedule:
    driftCheck: "*/5 * * * *"   # cron for the drift-check CronJob
    reconcile:  "0 * * * *"

  strategy:
    type: Enforce               # Audit | Once | Enforce

  sysctls:
    - name: net.core.rmem_max
      value: "16777216"
      persistent: true
    - name: net.core.somaxconn
      value: "65535"
      persistent: true
    - name: net.ipv4.tcp_congestion_control
      value: bbr
      persistent: true
    # Per-interface keys expand the {iface} placeholder across matching NICs:
    - name: net.ipv4.conf.{iface}.rp_filter
      value: "0"
      persistent: true
      interfaceSelector:
        prefixes: ["eth", "ens", "azure"]
        exclude: ["lo", "cilium_host", "cilium_net"]

  restoreOnDelete: true
```

> **Note:** the group `sysctl.k8s.io` is a protected API group, so the CRD
> carries the `api-approved.kubernetes.io` annotation. The worker pods run
> **privileged** with `hostNetwork/hostPID/hostIPC`, `dnsPolicy:
> ClusterFirstWithHostNet`, and the node's `/proc/sys` mounted in — so the
> target namespace must permit privileged pods (`kube-system` does by default).

## Getting Started

### Prerequisites
- Go v1.26+, Docker 17.03+, kubectl v1.11.3+, and access to a cluster.

### Build the images
```sh
# Manager
make docker-build IMG=<registry>/sysctl-operator:tag
# Worker (applier / drift-checker)
docker build -f Dockerfile.worker -t <registry>/sysctl-operator-worker:tag .
```
For a kind cluster you can skip a registry: `kind load docker-image ...`.

### Deploy
```sh
make install                                   # install the CRD
make deploy IMG=<registry>/sysctl-operator:tag # deploy the manager
```

### Sample usage
```sh
# Label the nodes you want tuned
kubectl label node <node> sysctlstack=tuned

# Apply a profile
kubectl apply -f config/samples/sysctl_v1alpha1_sysctlprofile.yaml

# Watch the rollout
kubectl -n kube-system get sysctlprofile aks-network-optimized -o wide
kubectl -n kube-system get cm,ds,cronjob -l sysctl.k8s.io/profile=aks-network-optimized

# Inspect status (per-node results, hashes, counts)
kubectl -n kube-system get sysctlprofile aks-network-optimized -o jsonpath='{.status}' | jq
```

Expected lifecycle: ConfigMap + applier DaemonSet appear → worker pods run
`sysctl -w` and report success → operator tears the DaemonSet down and sets
`status.teardownHash` → the drift CronJob keeps running on schedule.

### Uninstall
```sh
kubectl delete -f config/samples/sysctl_v1alpha1_sysctlprofile.yaml
make undeploy
make uninstall
```

## Notification (worker → operator report API)

Worker pods POST their per-node outcome to the manager's HTTP API
(`--report-bind-address`, default `:9090`). The manager records it onto the
profile's `status` (per-node `nodeStatuses`, `appliedNodes`/`failedNodes`,
`erroredPods`) and uses successful reports to decide teardown.

**Endpoint:** `POST /api/v1/reports`

**Payload:**
```json
{
  "profile":   "aks-network-optimized",
  "namespace": "kube-system",
  "node":      "node-1",
  "pod":       "aks-network-optimized-sysctl-abcde",
  "hash":      "9f1c2a3b4d5e6f70",
  "success":   false,
  "applied":   ["net.core.somaxconn"],
  "failed":    ["net.core.rmem_max: permission denied"],
  "message":   "1 sysctl failed"
}
```
`profile`, `namespace`, and `node` are required. A report for a deleted profile
is acknowledged (`200`) so stale pods stop retrying.

Try it manually (with the manager running):
```sh
curl -sS -XPOST localhost:9090/api/v1/reports -H 'Content-Type: application/json' \
  -d '{"profile":"aks-network-optimized","namespace":"kube-system","node":"node-1","success":false,"failed":["net.core.rmem_max"],"message":"denied"}'
```

The worker also **verifies the config hash** before acting: if the mounted
ConfigMap's hash differs from the `CONFIG_HASH` env the operator injected, the
worker aborts rather than apply a stale config.

## Metrics

Metrics register into controller-runtime's registry and are served on the
manager's `/metrics` endpoint.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `sysctl_operator_errored_pods_total` | Counter | `profile`, `namespace`, `node` | Applier/checker pods that reported an apply failure |

Every `success:false` report increments the counter for that profile/node (it's
a `CounterVec`, so a series appears only after the first failure). The same
events also bump `status.erroredPods` on the resource.

```sh
# Local (run the manager with HTTP metrics):
make run ARGS="--metrics-bind-address=:8080 --metrics-secure=false"
curl -sS localhost:8080/metrics | grep sysctl_operator_errored_pods_total
```

In-cluster, the scaffolded `ServiceMonitor` (`config/prometheus/monitor.yaml`)
scrapes the authn/authz-protected HTTPS metrics endpoint.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
