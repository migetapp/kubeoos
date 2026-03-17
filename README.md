# kubeoos

Kubernetes controller that automatically adds the `node.kubernetes.io/out-of-service` taint to NotReady nodes, enabling immediate pod evacuation and volume detachment.

## Why

When a Kubernetes node goes down, pods are marked for deletion but remain stuck in `Terminating` because the kubelet is unavailable to process the deletion. Volumes stay attached. Stateful workloads can't restart elsewhere.

Kubernetes 1.28+ supports the [`node.kubernetes.io/out-of-service`](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/#non-graceful-node-shutdown) taint which, when applied to a node, tells Kubernetes to force-detach volumes and allow pods to be deleted — but **no built-in controller applies this taint automatically**.

kubeoos fills this gap: it watches for NotReady nodes and, after a configurable timeout, applies the out-of-service taint. When the node recovers, the taint is removed.

## How It Works

```
Node goes NotReady
       │
       ▼
  Wait timeout (default 5m)
       │
       ▼
  Safety check: are too many nodes already tainted?
       │ no
       ▼
  Add node.kubernetes.io/out-of-service:NoExecute taint
       │
       ▼
  Kubernetes force-detaches volumes, deletes pods
       │
       ▼
  Pods reschedule on healthy nodes
       │
       ▼
  Node comes back Ready → taint removed automatically
```

## Features

- **Configurable timeout** — wait N minutes before declaring a node out of service (default: 5m)
- **Safety threshold** — won't taint more than N% of nodes (default: 49%), preventing cluster-wide evacuation
- **Automatic recovery** — removes the taint when a node returns to Ready
- **Leader election** — run multiple replicas for high availability
- **Control plane aware** — skips control plane nodes
- **Minimal footprint** — single binary, ~10m CPU / 32Mi memory

## Install

### Helm

```bash
helm install kubeoos oci://ghcr.io/migetapp/kubeoos/charts/kubeoos \
  --namespace kube-system
```

### Helm with custom values

```bash
helm install kubeoos oci://ghcr.io/migetapp/kubeoos/charts/kubeoos \
  --namespace kube-system \
  --set args.notReadyTimeout=3m \
  --set args.maxUnhealthyPercent=30
```

## Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `args.notReadyTimeout` | `5m` | How long a node must be NotReady before tainting |
| `args.maxUnhealthyPercent` | `49` | Max % of worker nodes that can be tainted (safety) |
| `args.checkInterval` | `30s` | Reconciliation interval |
| `args.logLevel` | `2` | klog verbosity (0-5) |
| `replicaCount` | `2` | Number of replicas (leader election ensures only one is active) |
| `leaderElection.enabled` | `true` | Enable leader election for HA |

## Comparison

| Tool | Approach | Reboots node? | Needs agent on failing node? | Needs IPMI/BMC? |
|------|----------|---------------|------------------------------|-----------------|
| **kubeoos** | Adds out-of-service taint from healthy node | No | No | No |
| SNR (medik8s) | Self-remediation via watchdog/reboot | Yes | Yes | No |
| FAR (medik8s) | Fence agent power-cycles node | Yes | No | Yes |
| kube-fencing | STONITH via fence agents | Yes | No | Yes |
| draino | Graceful drain (cooperative) | No | No | No |

## Requirements

- Kubernetes 1.28+ (for `NodeOutOfServiceVolumeDetach` feature gate, GA since 1.28)

## License

Apache License 2.0
