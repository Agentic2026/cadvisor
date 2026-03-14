# HTTP API Storage Driver

## Overview

The `httpapi` storage driver uploads collected cAdvisor container metrics to a remote HTTP API endpoint in batched JSON payloads. It operates as a native cAdvisor storage driver, sending data directly from the internal collection path rather than scraping cAdvisor's REST API or Prometheus endpoint.

## Enabling the Driver

Start cAdvisor with the `-storage_driver=httpapi` flag and configure the required environment variables:

```bash
export CADVISOR_METRICS_API_URL=https://your-metrics-api.example.com/v1/ingest
export CADVISOR_METRICS_API_TOKEN=your-secret-bearer-token

./cadvisor -storage_driver=httpapi -storage_driver_buffer_duration=15s
```

## Configuration

The driver is configured exclusively through environment variables. No additional command-line flags are required.

| Environment Variable | Required | Description |
|---|---|---|
| `CADVISOR_METRICS_API_URL` | Yes | The full URL of the remote HTTP API endpoint that will receive metric batches. |
| `CADVISOR_METRICS_API_TOKEN` | Yes | Bearer token used for authentication with the remote API. |

The standard cAdvisor flag `-storage_driver_buffer_duration` controls how frequently batches are flushed (default: 60s). Batches are also flushed when the internal batch size threshold is reached.

## Payload Format

The driver sends `POST` requests with `Content-Type: application/json`. Each request body is a JSON object with this structure:

```json
{
  "schema_version": 1,
  "sent_at": "2024-06-15T12:00:00Z",
  "machine_name": "host-01",
  "source": {
    "component": "cadvisor",
    "driver": "httpapi",
    "version": "v0.49.0"
  },
  "samples": [
    {
      "container_reference": {
        "id": "abc123",
        "name": "/docker/abc123",
        "aliases": ["my-container"],
        "namespace": "docker"
      },
      "container_spec": {
        "creation_time": "2024-01-01T00:00:00Z",
        "labels": {"app": "web"},
        "image": "nginx:latest"
      },
      "stats": {
        "timestamp": "2024-06-15T12:00:00Z",
        "cpu": { ... },
        "memory": { ... },
        ...
      }
    }
  ]
}
```

### Fields

- **`schema_version`**: Integer version of the payload schema (currently `1`).
- **`sent_at`**: UTC timestamp of when the batch was sent.
- **`machine_name`**: Hostname of the machine running cAdvisor.
- **`source`**: Metadata about the sending component.
- **`samples`**: Array of collected container stat snapshots.
  - **`container_reference`**: Container identity (name, ID, aliases, namespace).
  - **`container_spec`**: Container metadata (labels, image, creation time). Present when available.
  - **`stats`**: Full cAdvisor `ContainerStats` object with CPU, memory, network, filesystem, and other metrics. The `timestamp` field is the actual collection time.

## HTTP Request Details

- **Method**: `POST`
- **Headers**:
  - `Authorization: Bearer <CADVISOR_METRICS_API_TOKEN>`
  - `Content-Type: application/json`
  - `User-Agent: cAdvisor/<version> httpapi-storage`
- **Success**: Any `2xx` status code.
- **Failure**: Non-`2xx` responses are logged as errors with status code and truncated response body.

## Behavior

- **Non-blocking**: `AddStats` enqueues samples and returns immediately. Network I/O happens asynchronously in a background goroutine.
- **Batching**: Samples are batched by time (`-storage_driver_buffer_duration`) and by count (max 100 per batch).
- **Bounded memory**: The internal queue is bounded. When full, new samples are dropped and a warning is logged with drop counts.
- **Graceful shutdown**: On shutdown, remaining buffered samples are flushed synchronously before the driver exits.
- **Connection reuse**: A single `http.Client` with keep-alive is reused across all requests.
- **No automatic retries**: Failed POST requests are not retried to avoid duplicate data without an idempotency mechanism.

## Example

```bash
# Start cAdvisor with httpapi driver, flushing every 15 seconds
CADVISOR_METRICS_API_URL=https://metrics.example.com/v1/ingest \
CADVISOR_METRICS_API_TOKEN=my-secret-token \
./cadvisor \
  -storage_driver=httpapi \
  -storage_driver_buffer_duration=15s
```
