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

## Subdomain routing (`BASE_DOMAIN`) — recommended for SPAs

Apps with a fixed base path and an absolute `root_url` (Grafana, and most SPAs)
can't be reliably served under a path prefix like `/proxy/…/` — the app hardcodes
its base (`/grafana`) in HTML, bootData nav JSON, API responses and redirects, and
no amount of response rewriting keeps all of them consistent (you get doubled
paths like `/grafana/grafana/…`).

Subdomain routing fixes this by giving each service its **own origin** and
**preserving the path verbatim**:

```
https://‹slug›.BASE_DOMAIN/grafana/dashboards
        └── one service ──┘└── app's native path, untouched ──┘
```

Set `BASE_DOMAIN` (e.g. `kube-forwarder.devtron.ai`) in proxy mode. Then:

- The apex `BASE_DOMAIN` serves the peekaboo UI/API.
- Clicking a service calls `POST /api/links` and opens `https://‹slug›.BASE_DOMAIN/`,
  where `‹slug›` is `‹friendly-name›-‹hash›` (a stable DNS label derived from
  context+namespace+service+port).
- That subdomain is routed by Host header to the service over a **port-forward
  tunnel with the path preserved** — so the app's native `/grafana/…` paths, its
  `appSubUrl`, assets, API calls and WebSockets all just work. The only rewrite
  is pulling the app's initial `root_url` redirect back onto the slug origin.

Requires (all one-time infra):
- **Wildcard DNS** `*.BASE_DOMAIN` → the same ingress/LB as the apex.
- **Wildcard TLS** `*.BASE_DOMAIN` (e.g. cert-manager DNS-01).
- **Ingress** rule for host `*.BASE_DOMAIN` → the peekaboo Service (keep your IP
  allowlist on it).

TLS-serving backends (ports 443/6443/8443/9443 — e.g. argocd-server) are dialed
over HTTPS automatically; otherwise they'd 307-redirect HTTP→HTTPS forever.

Note: slug→service mappings are in-memory. After a pod restart, a cold bookmark
returns a friendly "re-open from the UI" message until you click the service
again (which re-registers it).

## Curated service list (`SERVICE_CONFIG`)

By default the service list shows *every* service in a namespace. Since the point
of this tool is reaching **UIs** (not headless/API/mesh services), you can curate
the list with a ConfigMap-driven JSON file, pointed to by the `SERVICE_CONFIG`
env var ([`deploy/configmap.yaml`](deploy/configmap.yaml)). Edit + re-apply the
ConfigMap and restart the deployment — no image rebuild.

Each entry selects a service by **label** (so it works despite per-cluster Helm
name prefixes), with optional name filters and a pinned port:

```json
{
  "requireEndpoints": true,
  "namespaces": {
    "monitoring": [
      { "name": "grafana",        "labelValue": "grafana" },
      { "name": "vmalertmanager", "labelValue": "vmalertmanager", "port": 9093 },
      { "name": "pyroscope",      "labelValue": "pyroscope", "excludeNameContains": ["headless","memberlist"] },
      { "name": "alloy-cluster",  "labelValue": "alloy", "nameContains": "-alloy-cluster" },
      { "name": "minio-console",  "labelKey": "app", "labelValue": "minio", "nameContains": "minio-console", "port": 9001 }
    ]
  }
}
```

- `labelKey` defaults to `app.kubernetes.io/name`; set it (e.g. `app`) for charts
  that use the legacy label.
- `nameContains` / `excludeNameContains` disambiguate services that share a label.
- `port` pins the exposed port (else the service's discovered ports are shown).
- `requireEndpoints` (default true) drops services with no endpoint IPs — this
  removes dead/duplicate services automatically (read via the core Endpoints API,
  so it works with a `view` token that can't read EndpointSlices).
- The UI shows the friendly `name`; the real service name appears beneath it.
- Namespaces not listed fall back to showing all their services.

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

### Tunnel mode for write-heavy apps (`TUNNEL_SERVICES`)

The API-server service-proxy path is subject to **per-method RBAC**: a `GET`
maps to the `get` verb on `services/proxy`, but a `POST` maps to `create`, which
many setups (e.g. Devtron) only grant to **Admin**. So with a read-only token you
can *view* an app but actions like **Grafana login (a POST)** get a 403.

To avoid needing Admin, services can be reached through a **port-forward tunnel**
instead: peekaboo runs `kubectl port-forward` to the service, binds a local port,
and reverse-proxies through it. A port-forward needs only the `pods/portforward`
permission (not Admin) and, once established, passes **all HTTP methods** through
transparently — so logins and other writes work.

Set the `TUNNEL_SERVICES` env var to a comma-separated list of glob patterns,
matched against `service` (or `namespace/service` if the pattern contains `/`):

```
TUNNEL_SERVICES=*grafana*                 # any service whose name contains "grafana"
TUNNEL_SERVICES=monitoring/*grafana*,foo/bar   # scoped / multiple patterns
```

Matching services use the tunnel; everything else stays on the stateless
service-proxy. Tunnels are started lazily, shared across requests for the same
service, rebuilt if the process dies, and reaped after ~5 minutes idle.

## Limitations

- **Proxy mode is HTTP/HTTPS only.** The service-proxy subresource speaks HTTP;
  it can't tunnel raw TCP protocols (Postgres, Redis, plain gRPC, etc.).
  Non-HTTP services are listed but won't render usefully when proxied.
- **Proxy mode needs the `services/proxy` RBAC verb** on the token (see above);
  the built-in `view` role doesn't include it. Write methods (POST/PUT/…) map to
  `create`/`update` and often need Admin — use `TUNNEL_SERVICES` for those.
- **Tunnel mode needs `pods/portforward`** and a service with ready pods; it
  pins to a single pod (bypassing Service load-balancing) and keeps a
  `kubectl port-forward` process alive per tunneled service.
- **Exec-plugin auth isn't supported in proxy mode** — the direct request path
  handles bearer tokens and client certs from the kubeconfig, not `exec`
  credential plugins.
- **No built-in auth.** In-cluster the `Service` is ClusterIP-only; put an
  IP-restricted ingress (or SSO) in front before exposing it.
- Selectorless services (e.g. the default `kubernetes` service) can't be
  port-forwarded in local mode; kubectl's error is surfaced verbatim.
