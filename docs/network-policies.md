# Network policies

This page documents every network connection made by the watcher and worker pods. Use it as a reference when writing Kubernetes `NetworkPolicy` resources.

## Watcher

### Egress

| Destination    | Protocol   | Port                                             | Required | Controlled by                                                 |
| -------------- | ---------- | ------------------------------------------------ | -------- | ------------------------------------------------------------- |
| Hatchet engine | gRPC (TCP)      | Encoded in `HATCHET_CLIENT_TOKEN` JWT claims     | Yes      | `HATCHET_CLIENT_TOKEN` (override: `HATCHET_CLIENT_HOST_PORT`) |
| OTLP collector | gRPC (TCP)      | `OTEL_EXPORTER_OTLP_ENDPOINT` (typically `4317`) | No       | `OTEL_EXPORTER_OTLP_ENDPOINT`                                 |
| kube-dns       | UDP+TCP         | `53`                                             | Yes      | Cluster DNS                                                   |

### Ingress

| Source          | Protocol   | Port                           | Notes                                                                   |
| --------------- | ---------- | ------------------------------ | ----------------------------------------------------------------------- |
| Kubelet         | HTTP (TCP) | `HEALTH_ADDR` (default `8081`) | Liveness (`/healthz`) and readiness (`/readyz`) probes. Always enabled. |
| Metrics scraper | HTTP (TCP) | `METRICS_ADDR`                 | `/metrics` endpoint. Only enabled when `METRICS_ADDR` is set.           |

## Worker

### Egress

| Destination         | Protocol            | Port                                             | Required | Controlled by                                                 |
| ------------------- | ------------------- | ------------------------------------------------ | -------- | ------------------------------------------------------------- |
| Hatchet engine      | gRPC (TCP)          | Encoded in `HATCHET_CLIENT_TOKEN` JWT claims     | Yes      | `HATCHET_CLIENT_TOKEN` (override: `HATCHET_CLIENT_HOST_PORT`) |
| Radarr              | HTTP or HTTPS (TCP) | Port in `RADARR_URL`                             | Yes      | `RADARR_URL`                                                  |
| Sonarr              | HTTP or HTTPS (TCP) | Port in `SONARR_URL`                             | Yes      | `SONARR_URL`                                                  |
| Poster image server | HTTP or HTTPS (TCP) | `80` / `443`                                     | No       | URL returned by Radarr/Sonarr API                             |
| OTLP collector      | gRPC (TCP)          | `OTEL_EXPORTER_OTLP_ENDPOINT` (typically `4317`) | No       | `OTEL_EXPORTER_OTLP_ENDPOINT`                                 |
| Webhook endpoint    | HTTP or HTTPS (TCP) | Port in `MEDIA_WEBHOOK_URL`                      | No       | `MEDIA_WEBHOOK_URL`                                           |
| kube-dns            | UDP+TCP             | `53`                                             | Yes      | Cluster DNS                                                   |

**Poster images:** the worker fetches artwork from URLs returned by the Radarr/Sonarr API. These URLs are typically relative paths served by the arr instance itself (same host and port as `RADARR_URL`/`SONARR_URL`), so no additional egress rule is needed in the common case. Some arr configurations include an external `RemoteURL` pointing to a CDN — if artwork fetch is enabled and your arr instance returns external image URLs, the worker will also connect to those hosts on port `443`.

### Ingress

| Source          | Protocol   | Port                           | Notes                                                                   |
| --------------- | ---------- | ------------------------------ | ----------------------------------------------------------------------- |
| Kubelet         | HTTP (TCP) | `HEALTH_ADDR` (default `8080`) | Liveness (`/healthz`) and readiness (`/readyz`) probes. Always enabled. |
| Metrics scraper | HTTP (TCP) | `METRICS_ADDR`                 | `/metrics` endpoint. Only enabled when `METRICS_ADDR` is set.           |

## TLS notes

**Hatchet:** The engine address and port are read from the JWT claims embedded in `HATCHET_CLIENT_TOKEN`. Set `HATCHET_CLIENT_HOST_PORT` to override them (useful when the address in the token does not resolve inside the cluster). TLS is enabled by default; set `HATCHET_CLIENT_TLS_STRATEGY=none` only when your engine runs without TLS. Your NetworkPolicy port rules are the same either way — TLS/plaintext runs on the same port.

**Radarr / Sonarr:** whether the connection is HTTP or HTTPS depends on the scheme in `RADARR_URL` and `SONARR_URL`. Typical self-hosted setups use plain HTTP within the cluster (e.g. `http://radarr:7878`), but HTTPS is also supported.

## Example NetworkPolicy

The following YAML sketch is a starting point. Adjust namespace selectors, labels, and ports to match your cluster topology. Replace `port: 7077` with whatever port your Hatchet engine gRPC endpoint listens on.

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
        - port: 9090
          protocol: TCP
  egress:
    # Hatchet engine gRPC
    - to:
        - podSelector:
            matchLabels:
              app: hatchet-engine
      ports:
        - port: 7077
          protocol: TCP
    # Optional: OTLP collector
    - to:
        - podSelector:
            matchLabels:
              app: otel-collector
      ports:
        - port: 4317
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
    # Hatchet engine gRPC
    - to:
        - podSelector:
            matchLabels:
              app: hatchet-engine
      ports:
        - port: 7077
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
    # Optional: OTLP collector
    - to:
        - podSelector:
            matchLabels:
              app: otel-collector
      ports:
        - port: 4317
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
