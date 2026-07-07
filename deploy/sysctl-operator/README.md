# sysctl-operator Helm chart

Deploys the sysctl-operator: a Kubernetes controller that applies **sysctl
parameters** (`SysctlProfile`) and **custom scripts** (`ScriptProfile`) to
selected nodes, then tears its worker DaemonSets down once every node reports
success.

## Prerequisites

- Kubernetes 1.25+
- The manager and worker images built and pushed to a registry reachable by the
  cluster (`make docker-build docker-push` for the manager; `Dockerfile.worker`
  for the worker).

## Install

> **Namespace is fixed.** The operator has its report URL compiled in as
> `http://sysctl-operator-report.sysctl-operator-system:9090`. Worker pods POST
> their status there, so the chart **must** be installed into the
> `sysctl-operator-system` namespace.

```bash
helm install sysctl-operator ./deploy/sysctl-operator \
  -n sysctl-operator-system --create-namespace
```

Override the image tag if needed:

```bash
helm install sysctl-operator ./deploy/sysctl-operator \
  -n sysctl-operator-system --create-namespace \
  --set image.tag=v8-amd64
```

## What gets installed

| Resource | Purpose |
|----------|---------|
| CRDs (`crds/`) | `SysctlProfile`, `ScriptProfile` |
| Deployment | The controller-manager (1 replica, leader-elected) |
| ServiceAccount + ClusterRole/Binding | Controller permissions |
| Role/RoleBinding | Leader-election lease permissions |
| Service `sysctl-operator-report` | Report API on :9090 the worker pods POST to |
| Service `*-metrics` | Prometheus metrics (only when `metrics.enabled=true`) |

## Key values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` / `image.tag` | `emirozbir/sysctl-operator` / `v8-amd64` | Manager image |
| `replicaCount` | `1` | Manager replicas |
| `leaderElection.enabled` | `true` | HA-safe single active manager |
| `metrics.enabled` | `false` | Serve Prometheus metrics |
| `metrics.secure` | `true` | HTTPS + authn/authz for metrics |
| `resources` | 10m/64Mi → 500m/128Mi | Manager pod resources |

See [`values.yaml`](./values.yaml) for the full list.

## Notes on CRDs

CRDs live in `crds/` and are installed automatically. Helm does **not** upgrade
CRDs on `helm upgrade`, nor delete them on `helm uninstall` (so existing
profiles survive). When the CRDs change, apply them manually:

```bash
kubectl apply -f deploy/sysctl-operator/crds/
```

## The worker image

The worker image the operator launches (applier DaemonSet, drift CronJob, script
runner) is currently **compiled into the operator** (`applierImage` in
`internal/controller`). The `worker.image.*` values are informational until the
operator is updated to read the image from configuration.

## Uninstall

```bash
helm uninstall sysctl-operator -n sysctl-operator-system
```

CRDs and any `SysctlProfile`/`ScriptProfile` objects are intentionally left in
place. Remove them explicitly if desired:

```bash
kubectl delete crd sysctlprofiles.sysctl.k8s.io scriptprofiles.sysctl.k8s.io
```
