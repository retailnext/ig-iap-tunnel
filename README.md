# ig-iap-tunnel

Selects a healthy instance from a GCP regional managed instance group and creates an IAP TCP tunnel to it, proxying local TCP connections through the tunnel to the remote instance.

## Usage

```
ig-iap-tunnel \
  --instance-group-id projects/{project}/regions/{region}/instanceGroups/{name} \
  --remote-port <port> \
  --local-port <port>
```

| Flag | Required | Description |
|------|----------|-------------|
| `--instance-group-id` | yes | Regional managed instance group resource ID |
| `--remote-port` | yes | Port on the remote instance to tunnel to |
| `--local-port` | yes | Local port to listen on (binds to `127.0.0.1`) |

## How It Works

1. Queries the GCP Compute API to list managed instances in the group.
2. Filters for healthy instances (`CurrentAction == NONE`, `InstanceStatus == RUNNING`, and passing health checks if configured).
3. Randomly selects one healthy instance.
4. Opens an IAP TCP tunnel to that instance via the `cedws/iapc` library.
5. Listens on `127.0.0.1:<local-port>` and proxies each incoming connection through the tunnel to `<remote-port>` on the selected instance.

The tool exits on `SIGINT` or `SIGTERM`, closing all active connections gracefully.

## Prerequisites

- Application Default Credentials configured (`gcloud auth application-default login`)
- IAP TCP forwarding enabled on the target instances
