# ig-iap-tunnel

Selects a healthy instance from a GCP regional managed instance group and creates an IAP TCP tunnel to it, proxying local TCP connections through the tunnel to the remote instance.

## Usage

```
ig-iap-tunnel \
  --instance-group-id projects/{project}/regions/{region}/instanceGroups/{name} \
  --remote-port <port> \
  --local-port <port> \
  [--proxy-domains example.com,internal.net]
```

| Flag | Required | Description |
|------|----------|-------------|
| `--instance-group-id` | yes | Regional managed instance group resource ID |
| `--remote-port` | yes | Port on the remote instance to tunnel to |
| `--local-port` | yes | Local port to listen on (binds to `127.0.0.1`) |
| `--proxy-domains` | no | Comma-separated domains to route through the IAP tunnel (see below) |

## How It Works

1. Queries the GCP Compute API to list managed instances in the group.
2. Filters for healthy instances (`CurrentAction == NONE`, `InstanceStatus == RUNNING`, and passing health checks if configured).
3. Randomly selects one healthy instance.
4. Opens an IAP TCP tunnel to that instance via the `cedws/iapc` library.
5. Listens on `127.0.0.1:<local-port>` and proxies each incoming connection through the tunnel to `<remote-port>` on the selected instance.

The tool exits on `SIGINT` or `SIGTERM`, closing all active connections gracefully.

## Selective Proxying (`--proxy-domains`)

By default every connection is forwarded through the IAP tunnel. When the remote port runs an HTTP proxy server (the typical `https_proxy=http://127.0.0.1:<local-port>` setup), `--proxy-domains` restricts which destinations use the tunnel:

- Each incoming connection is parsed as an HTTP proxy request (`CONNECT host:port` or an absolute-URI request).
- If the destination host matches a configured domain (exact match or subdomain, e.g. `example.com` matches `api.example.com`), the connection goes through the IAP tunnel to the remote proxy, byte-for-byte as the client sent it.
- Otherwise `ig-iap-tunnel` handles the request itself: `CONNECT` targets are dialed directly (replying `200 Connection Established`), and plain HTTP requests are forwarded to the origin server with `Connection: close`.

Notes:

- Routing is decided by the first request on each client connection. `CONNECT` connections are inherently single-destination; for plain HTTP the direct path forces one request per connection so every request is routed correctly.
- With `--proxy-domains` set, traffic on the local port must be HTTP proxy protocol; connections that don't parse as HTTP requests are dropped, and clients that send nothing (e.g. server-speaks-first protocols like SMTP) are dropped after a 10-second first-request timeout. Any TCP protocol wrapped in CONNECT by the client still works.

## Prerequisites

- Application Default Credentials configured (`gcloud auth application-default login`)
- IAP TCP forwarding enabled on the target instances
