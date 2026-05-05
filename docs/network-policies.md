# Network policies

This page documents every network connection made by the watcher and worker pods. Use it as a reference when writing Kubernetes `NetworkPolicy` resources.

## Watcher

### Egress

| Destination       | Protocol   | Port                                            | Required | Controlled by      |
| ----------------- | ---------- | ----------------------------------------------- | -------- | ------------------ |
| Temporal frontend | gRPC (TCP) | Port from `TEMPORAL_ADDRESS` (typically `7233`) | Yes      | `TEMPORAL_ADDRESS` |
| kube-dns          | UDP+TCP    | `53`                                            | Yes      | Cluster DNS        |

### Ingress

| Source          | Protocol   | Port                             | Notes                                                                   |
| --------------- | ---------- | -------------------------------- | ----------------------------------------------------------------------- |
| Kubelet         | HTTP (TCP) | `HEALTH_ADDR` (default `8081`)   | Liveness (`/healthz`) and readiness (`/readyz`) probes. Always enabled. |
| Metrics scraper | HTTP (TCP) | `METRICS_ADDR` (default `:9091`) | `/metrics` endpoint. Always enabled.                                    |

## Worker

### Egress

The table below lists the union of destinations across every activity. A worker pod registering only a subset of activities (helm `workers:` map, `WORKER_ACTIVITIES`) needs only the subset of destinations its activities use — see [Per-activity egress](#per-activity-egress).

| Destination         | Protocol            | Port                                            | Required | Controlled by                     |
| ------------------- | ------------------- | ----------------------------------------------- | -------- | --------------------------------- |
| Temporal frontend   | gRPC (TCP)          | Port from `TEMPORAL_ADDRESS` (typically `7233`) | Yes      | `TEMPORAL_ADDRESS`                |
| Radarr              | HTTP or HTTPS (TCP) | Port in `RADARR_URL`                            | Yes      | `RADARR_URL`                      |
| Sonarr              | HTTP or HTTPS (TCP) | Port in `SONARR_URL`                            | Yes      | `SONARR_URL`                      |
| Poster image server | HTTP or HTTPS (TCP) | `80` / `443`                                    | No       | URL returned by Radarr/Sonarr API |
| Webhook endpoint    | HTTP or HTTPS (TCP) | Port in `MEDIA_WEBHOOK_URL`                     | No       | `MEDIA_WEBHOOK_URL`               |
| kube-dns            | UDP+TCP             | `53`                                            | Yes      | Cluster DNS                       |

**Poster images:** the worker fetches artwork from URLs returned by the Radarr/Sonarr API. These URLs are typically relative paths served by the arr instance itself (same host and port as `RADARR_URL`/`SONARR_URL`), so no additional egress rule is needed in the common case. Some arr configurations include an external `RemoteURL` pointing to a CDN — if artwork fetch is enabled and your arr instance returns external image URLs, the worker will also connect to those hosts on port `443`.

### Per-activity egress

Every worker pod talks to Temporal and DNS; those rows are omitted from the matrix below. The remaining destinations are only needed when the listed activities are registered on the pod.

| Activity (token) | Radarr / Sonarr | Poster image server | Webhook endpoint | Notes                                                                                            |
| ---------------- | --------------- | ------------------- | ---------------- | ------------------------------------------------------------------------------------------------ |
| `probe`          | No              | No                  | No               | Local FFmpeg only.                                                                               |
| `detect-crop`    | No              | No                  | No               | Local FFmpeg only.                                                                               |
| `transcode`      | Yes             | Yes (if external)   | No               | Fetches cover art via `GetPosterImage`; also `GetInfo` when high-cardinality labels are enabled. |
| `notify`         | Yes             | No                  | No               | Triggers the arr library import via `ImportByFilePath`.                                          |
| `cleanup`        | No              | No                  | No               | Filesystem operations only.                                                                      |
| `notify-failure` | No              | No                  | Yes              | Sends the failure event to `MEDIA_WEBHOOK_URL`.                                                  |

A worker pod that registers only `probe`, `detect-crop`, or `cleanup` (alone or in combination) does not need Radarr/Sonarr or webhook egress. A worker that registers `notify-failure` is the only one that needs webhook egress.

### Ingress

| Source          | Protocol   | Port                             | Notes                                                                   |
| --------------- | ---------- | -------------------------------- | ----------------------------------------------------------------------- |
| Kubelet         | HTTP (TCP) | `HEALTH_ADDR` (default `8080`)   | Liveness (`/healthz`) and readiness (`/readyz`) probes. Always enabled. |
| Metrics scraper | HTTP (TCP) | `METRICS_ADDR` (default `:9090`) | `/metrics` endpoint. Always enabled.                                    |

## TLS notes

**Radarr / Sonarr:** whether the connection is HTTP or HTTPS depends on the scheme in `RADARR_URL` and `SONARR_URL`. Typical self-hosted setups use plain HTTP within the cluster (e.g. `http://radarr:7878`), but HTTPS is also supported.

## Example NetworkPolicy

The following YAML sketch is a starting point. Adjust namespace selectors, labels, and ports to match your cluster topology. Replace `port: 7233` with whatever port your Temporal frontend gRPC endpoint listens on.

```yaml
# Watcher
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: watcher
spec:
  podSelector:
    matchLabels:
      app: watcher
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # Health probes from the kubelet (node-level traffic)
    - ports:
        - port: 8081
          protocol: TCP
    # Optional: Prometheus scraper
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: 9091
          protocol: TCP
  egress:
    # Temporal frontend gRPC
    - to:
        - podSelector:
            matchLabels:
              app: temporal-frontend
      ports:
        - port: 7233
          protocol: TCP
    # DNS
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
---
# Worker
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: worker
spec:
  podSelector:
    matchLabels:
      app: worker
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # Health probes from the kubelet
    - ports:
        - port: 8080
          protocol: TCP
    # Optional: Prometheus scraper
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: 9090
          protocol: TCP
  egress:
    # Temporal frontend gRPC
    - to:
        - podSelector:
            matchLabels:
              app: temporal-frontend
      ports:
        - port: 7233
          protocol: TCP
    # Radarr
    - to:
        - podSelector:
            matchLabels:
              app: radarr
      ports:
        - port: 7878
          protocol: TCP
    # Sonarr
    - to:
        - podSelector:
            matchLabels:
              app: sonarr
      ports:
        - port: 8989
          protocol: TCP
    # Optional: webhook and external poster images (if applicable)
    - ports:
        - port: 443
          protocol: TCP
        - port: 80
          protocol: TCP
    # DNS
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
```
