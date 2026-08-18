# kube-forwarder

A small web app to reach Kubernetes services. Drill down
**Cluster → Namespace → Service**, click a port, and connect. It runs in two
modes:

- **`portforward` (local, default):** shells out to `kubectl port-forward` and
  gives you a `localhost:PORT`. This is the local-dev experience.
- **`proxy` (hosted):** reverse-proxies your browser to the service **through
  the selected cluster's API server** (the `services/proxy` subresource). This
  reaches services in *any* cluster the mounted kubeconfig can talk to — not
  just the cluster the pod runs in — and returns through the app's single HTTP
  port, so it works behind one ingress.

The mode is set with the `FORWARD_MODE` env var and the UI adapts to it.

- **Backend**: Go (stdlib only). Uses `kubectl` for *listing*; proxy mode builds
  the authenticated API-server request directly (`httputil.ReverseProxy` + the
  kubeconfig's server URL and token).
- **Frontend**: React + Vite, embedded into the Go binary at build time.

## Why two modes

`kubectl port-forward` binds the forwarded port to the machine running it. On
your laptop that's reachable; inside a pod it binds to the *pod's* localhost,
which a remote browser can't reach.

Proxy mode instead routes each request through the API server's service-proxy
subresource: `GET {apiserver}/api/v1/namespaces/{ns}/services/{svc}:{port}/proxy/...`.
Because it uses the same API endpoint the kubeconfig already talks to, it works
across clusters (including through a management proxy such as a Devtron
`.../orchestrator/k8s/proxy/cluster/<name>` server URL). The request is built in
Go so we control the headers — in particular we omit `X-Forwarded-For`, which
some API gateways reject when it carries a loopback/private IP.

### RBAC for proxy mode

The token in the mounted kubeconfig needs the **`services/proxy`** subresource,
which the built-in `view` ClusterRole does *not* grant. Add it, scoped to the
namespaces you expose:

```yaml
apiGroups: [""]
resources: ["services/proxy"]
verbs: ["get", "create"]
```

## Requirements

- `kubectl` on your `PATH`, already configured (this app uses your existing
  contexts and credentials — it never stores anything).
- Go 1.22+ and Node 18+ (only to build).

## Run (production: single binary)

```bash
# 1. build the frontend into backend/web
cd frontend && npm install && npm run build

# 2. build & run the Go server (serves the embedded UI + API)
cd ../backend && go run .
```

Then open http://127.0.0.1:8770.

To produce a standalone binary:

```bash
cd backend && go build -o kube-forwarder .
./kube-forwarder            # or -addr 127.0.0.1:9000 to change the port
```

## Run (development: hot reload)

Two terminals:

```bash
# terminal 1 — backend API
cd backend && go run .

# terminal 2 — Vite dev server (proxies /api to the backend)
cd frontend && npm run dev
```

Open the URL Vite prints (http://127.0.0.1:5183).

## Deploy to Kubernetes (proxy mode)

The app runs **inside the target cluster** (e.g. the cd.example.com cluster) and
talks to the API using a mounted, restricted kubeconfig. Manifests live in
[`deploy/`](deploy/).

```bash
# 1. Build and push the image (your CI can do this too).
docker build -t REGISTRY/kube-forwarder:TAG .
docker push  REGISTRY/kube-forwarder:TAG
# then set that image ref in deploy/deployment.yaml

# 2. Create the kubeconfig Secret from your restricted kubeconfig file.
kubectl create secret generic kube-forwarder-kubeconfig \
  --from-file=config=/path/to/restricted-kubeconfig \
  -n <target-namespace>

# 3. Apply the workload.
kubectl apply -n <target-namespace> -f deploy/deployment.yaml
kubectl apply -n <target-namespace> -f deploy/service.yaml

# 4. Expose it via your own IP-restricted ingress (template provided).
kubectl apply -n <target-namespace> -f deploy/ingress.example.yaml
```

Notes on the manifests:

- The container defaults to `FORWARD_MODE=proxy`, `LISTEN_ADDR=0.0.0.0:8770`,
  `KUBECONFIG=/etc/kube/config`.
- The `Service` is **ClusterIP** (internal only). Public access is entirely up
  to the ingress in front — [`deploy/ingress.example.yaml`](deploy/ingress.example.yaml)
  shows an nginx `whitelist-source-range` IP allowlist; adjust the host, class,
  and CIDRs.
- The pod runs as non-root with a read-only root filesystem; `kubectl`'s cache
  goes to an `emptyDir` mounted at `/tmp`.

## How it works

| Endpoint | Purpose |
|---|---|
| `GET /api/config` | runtime config, chiefly `forwardMode` |
| `GET /api/contexts` | kubeconfig contexts (shown as "clusters") |
| `GET /api/namespaces?context=` | namespaces in a context |
| `GET /api/services?context=&namespace=` | services + their ports |
| `GET /proxy/{context}/{ns}/{svc}/{port}/...` | **proxy mode:** proxy to the service via that context's API server |
| `POST /api/forwards` | **portforward mode:** start a `kubectl port-forward` |
| `GET /api/forwards` | list running forwards |
| `DELETE /api/forwards/{id}` | stop a forward |

In **portforward mode**, local port is left to `0` so `kubectl` picks a free
port; the backend parses kubectl's `Forwarding from 127.0.0.1:PORT` line and
reports it to the UI. Forwards are in-memory: stopping the server stops them.

In **proxy mode**, clicking a port opens
`/proxy/{context}/{ns}/{svc}/{port}/` in a new tab; the app looks up that
context's server URL + token from the kubeconfig and relays the request through
the API server's service-proxy subresource. Ports 443/6443/8443/9443 are
proxied as HTTPS upstreams; everything else as HTTP.

### Sub-path apps (e.g. Grafana)

Some apps are configured to live at a fixed base path — Grafana with
`serve_from_sub_path: true` and `root_url: https://host/grafana/` insists on
being served under `/grafana/` and redirects everything else there. To make
these work under the proxy prefix, proxy mode does **best-effort** response
rewriting:

- redirect `Location` headers are pulled back under the proxy prefix (so the
  browser isn't bounced to the app's external `root_url`);
- in HTML responses, the API server's own service-proxy base path is translated
  to the proxy prefix, and Grafana's `appSubUrl` (in bootData) is prefixed too;
- `Set-Cookie` `Path` is rewritten to the proxy prefix.

This is best-effort: it covers redirects, HTML asset/API links, and cookies, but
not every URL an SPA may build at runtime (some live/WebSocket features may still
misbehave). Verified working for Grafana 11.x served this way.

## Limitations

- **Proxy mode is HTTP/HTTPS only.** The service-proxy subresource speaks HTTP;
  it can't tunnel raw TCP protocols (Postgres, Redis, plain gRPC, etc.).
  Non-HTTP services are listed but won't render usefully when proxied.
- **Proxy mode needs the `services/proxy` RBAC verb** on the token (see above);
  the built-in `view` role doesn't include it.
- **Exec-plugin auth isn't supported in proxy mode** — the direct request path
  handles bearer tokens and client certs from the kubeconfig, not `exec`
  credential plugins.
- **No built-in auth.** In-cluster the `Service` is ClusterIP-only; put an
  IP-restricted ingress (or SSO) in front before exposing it.
- Selectorless services (e.g. the default `kubernetes` service) can't be
  port-forwarded in local mode; kubectl's error is surfaced verbatim.
