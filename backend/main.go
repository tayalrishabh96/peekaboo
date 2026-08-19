package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"

	"kube-forwarder/internal/kube"
)

//go:embed all:web
var embeddedWeb embed.FS

// envOr returns the value of env var key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	// Env vars take precedence so the container can be configured without args.
	defaultAddr := envOr("LISTEN_ADDR", "127.0.0.1:8770")
	addr := flag.String("addr", defaultAddr, "address the HTTP server listens on (env LISTEN_ADDR)")
	flag.Parse()

	// Address that `kubectl port-forward` binds forwarded ports to (only used in
	// "portforward" mode). Locally 127.0.0.1 is fine.
	kube.ForwardBindAddress = envOr("FORWARD_BIND_ADDRESS", "127.0.0.1")

	// How the "connect to a service" step works:
	//   "proxy"       — reverse-proxy to the in-cluster service over cluster DNS
	//                    (correct when running inside the target cluster)
	//   "portforward" — spawn `kubectl port-forward` (correct for local dev)
	forwardMode := envOr("FORWARD_MODE", "portforward")

	// Optional curated service list (ConfigMap-driven). When set and readable,
	// only listed UI services are shown for configured namespaces.
	var svcConfig *kube.ServiceConfig
	if p := os.Getenv("SERVICE_CONFIG"); p != "" {
		if c, err := kube.LoadServiceConfig(p); err != nil {
			log.Printf("service config %s: %v (listing all services)", p, err)
		} else {
			svcConfig = c
			log.Printf("loaded curated service config from %s (%d namespaces)", p, len(c.Namespaces))
		}
	}

	mgr := kube.NewForwardManager()
	api := &API{mgr: mgr, forwardMode: forwardMode, svcConfig: svcConfig}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", api.getConfig)
	mux.HandleFunc("GET /api/contexts", api.listContexts)
	mux.HandleFunc("GET /api/namespaces", api.listNamespaces)
	mux.HandleFunc("GET /api/services", api.listServices)
	mux.HandleFunc("GET /api/forwards", api.listForwards)
	mux.HandleFunc("POST /api/forwards", api.startForward)
	mux.HandleFunc("DELETE /api/forwards/{id}", api.stopForward)

	// Reverse-proxy endpoint for "proxy" mode. TUNNEL_SERVICES is a
	// comma-separated list of glob patterns (matched against "service" or
	// "namespace/service"); matching services are reached via a port-forward
	// tunnel (all HTTP methods, no Admin RBAC) instead of the API server.
	if forwardMode == "proxy" {
		tunnelServices := envOr("TUNNEL_SERVICES", "")
		if tunnelServices != "" {
			log.Printf("tunneling services matching: %s", tunnelServices)
		}
		mux.Handle(kube.ProxyPrefix, kube.NewServiceProxy(tunnelServices))
	}

	// Serve the embedded frontend (built React app). Falls back to a message
	// when the app hasn't been built yet.
	if sub, err := fs.Sub(embeddedWeb, "web"); err == nil {
		if _, statErr := fs.Stat(sub, "index.html"); statErr == nil {
			mux.Handle("/", spaHandler(sub))
		} else {
			mux.Handle("/", placeholderHandler())
		}
	}

	log.Printf("kube-forwarder listening on http://%s", *addr)
	log.Printf("reading kubeconfig from %s", kube.ConfigPath())
	srv := &http.Server{Addr: *addr, Handler: withCORS(mux)}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// spaHandler serves static files and falls back to index.html for client-side routes.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(fsys, trimLeadingSlash(r.URL.Path)); err != nil && r.URL.Path != "/" {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}

func trimLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	if p == "" {
		return "."
	}
	return p
}

func placeholderHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body style="font-family:sans-serif;padding:2rem">
<h1>kube-forwarder backend is running</h1>
<p>The frontend has not been built yet. Run <code>npm run build</code> in the <code>frontend</code> directory,
then copy <code>frontend/dist</code> to <code>backend/web</code>, or run the frontend dev server with <code>npm run dev</code>.</p>
<p>API is live at <code>/api/contexts</code>.</p></body></html>`)
	})
}

// withCORS allows the Vite dev server (different port) to call the API.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// API holds the HTTP handlers.
type API struct {
	mgr         *kube.ForwardManager
	forwardMode string
	svcConfig   *kube.ServiceConfig
}

// getConfig exposes runtime config the frontend needs, chiefly how the
// "connect to a service" step should behave.
func (a *API) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"forwardMode": a.forwardMode,
		"proxyPrefix": kube.ProxyPrefix,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (a *API) listContexts(w http.ResponseWriter, r *http.Request) {
	ctxs, err := kube.ListContexts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ctxs)
}

func (a *API) listNamespaces(w http.ResponseWriter, r *http.Request) {
	ctx := r.URL.Query().Get("context")
	if ctx == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("context query param is required"))
		return
	}
	ns, err := kube.ListNamespaces(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (a *API) listServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.URL.Query().Get("context")
	ns := r.URL.Query().Get("namespace")
	if ctx == "" || ns == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("context and namespace query params are required"))
		return
	}
	svcs, err := kube.ListServices(ctx, ns, a.svcConfig)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, svcs)
}

func (a *API) listForwards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.mgr.List())
}

func (a *API) startForward(w http.ResponseWriter, r *http.Request) {
	var req kube.ForwardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	fwd, err := a.mgr.Start(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, fwd)
}

func (a *API) stopForward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.mgr.Stop(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
