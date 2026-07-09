# Observability — dashboards & alerts (canonical)

This directory is the **canonical source of truth** for EdgeQuota's operational
dashboard and alerting rules. It replaces the retired `deploy/grafana/` (Grafana
JSON) and `deploy/prometheus/` (Prometheus scrape rules) — both dead since
EdgeQuota moved from scraped Prometheus metrics to **portable OpenTelemetry**
(v0.11.0): dotted semconv metric names, OTLP push, no `/metrics` endpoint.

The folder name is deliberately vendor-neutral (matching `internal/observability`):
the metric *emission* is backend-agnostic. Only the query dialect below targets
the current OTLP consumer.

## Contents

| File | What it is |
| --- | --- |
| `dashboard.yaml` | Operational dashboard (Perses/Dash0 schema, JSON body). |
| `alerts.yaml` | Alerting rules (Prometheus rule format). |

## Surfaced-name convention (how to write queries)

EdgeQuota emits dotted OTel names with semconv attributes. An OTLP backend
surfaces them in PromQL as:

- **metric name** → matched via `otel_metric_name`, **dotted and verbatim** —
  no `_total` / `_bucket` suffix. One matcher per query:
  `{otel_metric_name="edgequota.requests"}`.
- **attribute keys** → dots become underscores:
  `edgequota.status_class` → `edgequota_status_class`.
- **histograms** → add `otel_metric_type="histogram"` to the selector;
  `histogram_quantile(…, sum by (le)(rate(…)))` works unchanged.
- **shared names** (`http.server.request.duration`, `http.server.active_requests`,
  `go.goroutine.count`) are emitted by more than one service → scope by
  `service_name="edgequota"` or `k8s_deployment_name="edgequota"`.

Traffic / error / no-traffic panels and alerts key off **`edgequota.requests`**
(the unconditional terminal counter that spans streaming — SSE/WS/gRPC), not
`http.server.request.duration._count`, which structurally excludes streaming.

`go.goroutine.count` and the other `go.*` runtime metrics come from the OTel
runtime instrumentation (`runtime.Start`), which replaced the Prometheus Go
collector removed in the clean cut. `EdgeQuotaNoTraffic` uses that gauge's
presence as its liveness guard (the old `build_info` gauge was dropped).

## Sync model — infra mirrors this repo

The platform observability stack applies these via
`shoro/infrastructure .../terraform/environments/08_dash0`:

- `dashboard.yaml` is copied **byte-for-byte** to
  `08_dash0/dashboards/edgequota.yaml` (loaded with `file()`).
- `alerts.yaml` is mirrored into the `EdgeQuota*` `check_rule` block in
  `08_dash0/main.tf`, adapted to Dash0's `$__threshold` model (thresholds moved
  out of the expression into a field). Rule names, metric selection, `for`, and
  severities stay in lockstep.

**This repo leads; infra must never be ahead.** When a rule or panel changes
here, update the infra mirror in the same change and verify parity
(`diff` the dashboards; the alert set and queries must match).
