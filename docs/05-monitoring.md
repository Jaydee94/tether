# Monitoring with Prometheus & Grafana

This document covers exposing metrics, Prometheus scrape configurations, Grafana dashboards, and alerting recommendations for Tether.

## Metrics

Tether exposes Prometheus-formatted metrics from the operator and proxy. Typical endpoints:

- `:9090/metrics` for the operator
- `:9180/metrics` for the proxy

## Prometheus Scrape Configuration

Add a scrape job to your Prometheus config (or use ServiceMonitor with the Prometheus Operator):

```yaml
- job_name: "tether-operator"
  static_configs:
    - targets: ["tether-operator.tether-system.svc.cluster.local:9090"]

- job_name: "tether-proxy"
  static_configs:
    - targets: ["tether-proxy.tether-system.svc.cluster.local:9180"]
```

## Grafana Dashboards

Provide dashboards for:

- Lease lifecycle (active, pending, expired)
- Proxy connections and error rates
- Operator reconciliation latencies

Example panels:

- `tether_lease_active_count` — active leases by namespace
- `tether_proxy_connections_total` — total connections

## Alerting

Suggested alerts:

- High rate of lease failures (`tether_lease_errors_total > 0`)\n- Proxy high error rate or connection drops\n- Operator reconciliation errors or crashes

## Troubleshooting

- If metrics are missing, ensure the service targets are resolvable from Prometheus
- Check the operator and proxy logs for errors exposing metrics
