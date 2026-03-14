# Exporting cAdvisor Stats to a Remote HTTP API

cAdvisor supports exporting container stats to any HTTP endpoint via the `httpapi` storage driver.
Unlike the Prometheus endpoint (which is pull-based), this driver **pushes** batched JSON payloads
directly from the native storage-driver path — no scraping, no sidecar, no self-calls to cAdvisor's
own `/api` or `/metrics` endpoints.

## How it works

1. For every container housekeeping cycle, `AddStats` is called on the driver.  
   The driver enqueues a snapshot of the stats immediately and returns — **no blocking network I/O
   on the hot path**.

2. A background goroutine holds an internal batch.  It flushes the batch (via HTTP POST) when
   either of two conditions is met:
   - The `storage_driver_buffer_duration` timer fires (default: 60 s), **or**
   - The batch reaches an internal sample-count threshold (500 samples).

3. Payloads are JSON-encoded `batchEnvelope` objects containing one sample per container/tick.
   Each sample includes `container_reference`, `container_spec` (when available), and `stats`.

4. On shutdown (`Close()`), the background goroutine drains any remaining buffered samples and
   performs a final synchronous flush before returning.

## Enabling the driver

Set the required environment variables and pass `-storage_driver=httpapi`:

```sh
export CADVISOR_METRICS_API_URL="https://ingest.example.com/cadvisor/batch"
export CADVISOR_METRICS_API_TOKEN="your-bearer-token-here"

./cadvisor -storage_driver=httpapi -storage_driver_buffer_duration=15s
```

### Required environment variables

| Variable | Description |
|---|---|
| `CADVISOR_METRICS_API_URL` | Full URL that will receive the POST requests. |
| `CADVISOR_METRICS_API_TOKEN` | Bearer token sent in the `Authorization` header. |

> **cAdvisor will refuse to start** with `-storage_driver=httpapi` if either variable is missing.

### Optional flag

| Flag | Default | Description |
|---|---|---|
| `-storage_driver_buffer_duration` | `60s` | How long stats are buffered before being flushed to the remote API. |

## Payload schema

Each POST has `Content-Type: application/json` and the following shape:

```json
{
  "schema_version": "1",
  "sent_at": "2024-01-15T12:34:56.789Z",
  "machine_name": "node-hostname",
  "source": {
    "component": "cadvisor",
    "driver": "httpapi",
    "version": "v0.50.0"
  },
  "samples": [
    {
      "container_reference": {
        "name": "/docker/abc123",
        "aliases": ["my-container"],
        "namespace": "docker"
      },
      "container_spec": {
        "image": "nginx:latest",
        "labels": { "com.example.app": "web" }
      },
      "stats": {
        "timestamp": "2024-01-15T12:34:55.000Z",
        "cpu": { ... },
        "memory": { ... },
        ...
      }
    }
  ]
}
```

- `stats.timestamp` preserves the collection time from cAdvisor — it is **not** replaced with
  the upload time (`sent_at`).
- `container_spec` is omitted (`null`) when cAdvisor has not yet populated spec metadata for
  the container.
- The driver does not retry failed requests. Non-2xx responses are logged with a truncated
  snippet of the response body. Operator visibility is maintained via klog.

## HTTP semantics

| Property | Value |
|---|---|
| Method | `POST` |
| `Authorization` | `Bearer <CADVISOR_METRICS_API_TOKEN>` |
| `Content-Type` | `application/json` |
| `User-Agent` | `cAdvisor/<version> httpapi-storage` |
| Redirects | **Not followed** — a 3xx is treated as an error |
| Connection reuse | Yes — a single `http.Transport` is shared across all requests |
| Request timeout | 30 s |

## Memory & delivery semantics

- The sample queue is bounded (capacity: 2000 samples).  When the queue is full, new samples
  are **dropped** and a warning is logged. This prevents unbounded memory growth when the
  remote API is slow or unavailable.
- Batched requests are not automatically retried. If idempotent retries are required, include
  an idempotency key in your backend and replay the batch on application restart.

## Example: Docker run

```sh
docker run \
  --volume=/:/rootfs:ro \
  --volume=/var/run:/var/run:ro \
  --volume=/sys:/sys:ro \
  --volume=/var/lib/docker/:/var/lib/docker:ro \
  -e CADVISOR_METRICS_API_URL=https://ingest.example.com/cadvisor/batch \
  -e CADVISOR_METRICS_API_TOKEN=my-secret-token \
  --publish=8080:8080 \
  gcr.io/cadvisor/cadvisor:latest \
  -storage_driver=httpapi \
  -storage_driver_buffer_duration=15s
```
