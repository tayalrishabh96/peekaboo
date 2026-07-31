# kube-forwarder

A small web app to reach Kubernetes services. Drill down
**Cluster → Namespace → Service**, click a port, and connect. It runs in two
modes:

- **`portforward` (local, default):** shells out to `kubectl port-forward` and
  gives you a `localhost:PORT`. This is the local-dev experience.
- **`proxy` (in-cluster):** reverse-proxies your browser straight to the
  in-cluster service over cluster DNS
  (`service.namespace.svc.cluster.local:port`). No port-forward — traffic
  returns through the app's single HTTP port, so it works behind an ingress.

The mode is set with the `FORWARD_MODE` env var and the UI adapts to it.

- **Backend**: Go (stdlib only). Uses `kubectl` for *listing*; the proxy is
  pure Go (`httputil.ReverseProxy`).
- **Frontend**: React + Vite, embedded into the Go binary at build time.

## Why two modes

`kubectl port-forward` binds the forwarded port to the machine running it. On
your laptop that's reachable; inside a pod it binds to the *pod's* localhost,
which a remote browser can't reach. When the app runs **in the same cluster** as
the services, it doesn't need a forward at all — it can dial the service
directly and relay the response. That's proxy mode, and it's the right model for
the hosted deployment.

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
| `GET /proxy/{ns}/{svc}/{port}/...` | **proxy mode:** reverse-proxy to the service |
| `POST /api/forwards` | **portforward mode:** start a `kubectl port-forward` |
| `GET /api/forwards` | list running forwards |
| `DELETE /api/forwards/{id}` | stop a forward |

In **portforward mode**, local port is left to `0` so `kubectl` picks a free
port; the backend parses kubectl's `Forwarding from 127.0.0.1:PORT` line and
reports it to the UI. Forwards are in-memory: stopping the server stops them.

In **proxy mode**, clicking a port opens `/proxy/{ns}/{svc}/{port}/` in a new
tab and the app relays the request to the service.

## Limitations

- **Proxy mode is HTTP/HTTPS only.** A browser reverse-proxy can't tunnel raw
  TCP protocols (Postgres, Redis, gRPC-over-h2c without TLS handling, etc.).
  Non-HTTP services are listed but won't render usefully when proxied.
- **Proxy mode only reaches services in the pod's own cluster** — that's what
  cluster DNS resolves. Selecting a service from a *different* kubeconfig
  context in the list is fine for browsing, but proxying assumes it lives in the
  cluster the pod runs in.
- **No built-in auth.** In-cluster the `Service` is ClusterIP-only; put an
  IP-restricted ingress (or SSO) in front before exposing it.
- Selectorless services (e.g. the default `kubernetes` service) can't be
  port-forwarded in local mode; kubectl's error is surfaced verbatim.
