package kube

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// ProxyPrefix is the URL path under which service traffic is reverse-proxied:
//   /proxy/{namespace}/{service}/{port}/...  ->  http://service.namespace.svc:port/...
const ProxyPrefix = "/proxy/"

// ServiceProxy reverse-proxies browser requests to in-cluster services over
// cluster DNS. This replaces kubectl port-forward when running inside the
// cluster: the pod can reach any ClusterIP service directly, and everything
// flows back through this one HTTP port (so an ingress in front just works).
type ServiceProxy struct{}

func NewServiceProxy() *ServiceProxy { return &ServiceProxy{} }

// ServeHTTP handles /proxy/{namespace}/{service}/{port}/{rest...}.
func (p *ServiceProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, ProxyPrefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		http.Error(w, "expected /proxy/{namespace}/{service}/{port}/...", http.StatusBadRequest)
		return
	}
	namespace, service, portStr := parts[0], parts[1], parts[2]
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	upstreamPath := "/"
	if len(parts) == 4 {
		upstreamPath = "/" + parts[3]
	}

	// Resolve via cluster DNS. Only reaches services in the pod's own cluster.
	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s.%s.svc.cluster.local:%d", service, namespace, port),
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, e error) {
		http.Error(rw, "upstream unreachable: "+e.Error(), http.StatusBadGateway)
	}

	// Rewrite the incoming request so the upstream sees the sub-path, not the
	// /proxy/... prefix.
	r.URL.Path = upstreamPath
	r.Host = target.Host
	proxy.ServeHTTP(w, r)
}
