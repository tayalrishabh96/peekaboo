package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ctxKey is the type for values stashed in the request context.
type ctxKey int

// metaKey holds per-request rewrite info so ModifyResponse can map upstream
// URLs back into our proxy prefix.
const metaKey ctxKey = 0

// proxyMeta carries the two path strings ModifyResponse needs:
//   prefix  — browser-facing base, /proxy/{ctx}/{ns}/{svc}/{port}
//   apiBase — the API server's own service-proxy base,
//             /api/v1/namespaces/{ns}/services/{svcRef}/proxy
// The API server rewrites absolute links in proxied HTML to start with apiBase;
// we translate that back to prefix so the browser stays within peekaboo.
type proxyMeta struct {
	prefix  string
	apiBase string
}

// Rewrite patterns for making a sub-path app (e.g. Grafana with an absolute
// root_url + serve_from_sub_path) work under our proxy prefix. Best-effort:
// covers the common HTML/redirect/cookie cases, not every runtime-built URL.
var (
	// Grafana embeds its base path as "appSubUrl":"/grafana" in bootData JSON;
	// neither path rewrites this, so we prepend the prefix ourselves.
	reAppSubURL = regexp.MustCompile(`("appSubUrl"\s*:\s*")(/[^"]*)`)
	// Set-Cookie Path attribute.
	reCookiePath = regexp.MustCompile(`(?i)(;\s*[Pp]ath=)(/[^;]*)`)
	// Absolute-path attribute values href="/x", src="/x", action="/x" (single
	// leading slash, so protocol-relative //host is left alone). Used in tunnel
	// mode, where the upstream is the app directly (no API-server rewriting).
	reAttrAbsPath = regexp.MustCompile(`(?i)((?:href|src|action)=["'])(/[^/][^"']*)`)
)

// ProxyPrefix is the URL path under which service traffic is reverse-proxied:
//   /proxy/{context}/{namespace}/{service}/{port}/...
//
// Requests are routed through the Kubernetes API server's service-proxy
// subresource, so they reach services in ANY cluster the kubeconfig can talk to
// (not just the pod's own cluster). We build the authenticated request directly
// (rather than via `kubectl proxy`) so we control the headers — notably we do
// NOT send X-Forwarded-For, which some API gateways (e.g. Devtron's nginx in
// front of cd.example.com) reject when it contains a loopback/private IP.
const ProxyPrefix = "/proxy/"

// httpsProxyPorts get an "https:" scheme prefix on the service-proxy path so the
// API server dials the backend over TLS. Everything else defaults to http.
var httpsProxyPorts = map[int]bool{443: true, 6443: true, 8443: true, 9443: true}

// ServiceProxy reverse-proxies browser requests to cluster services. By default
// it routes through the selected context's API server (service-proxy). Services
// matching a tunnel pattern instead go through a port-forward tunnel, which
// supports all HTTP methods without Admin RBAC.
type ServiceProxy struct {
	mu             sync.Mutex
	byCtx          map[string]*httputil.ReverseProxy
	tunnelPatterns []string
	tunnels        *tunnelManager
}

// NewServiceProxy builds the proxy. tunnelServices is a comma-separated list of
// glob patterns (matched against "service" or "namespace/service") whose
// traffic should go through a port-forward tunnel instead of the API server.
func NewServiceProxy(tunnelServices string) *ServiceProxy {
	var pats []string
	for _, p := range strings.Split(tunnelServices, ",") {
		if s := strings.TrimSpace(p); s != "" {
			pats = append(pats, s)
		}
	}
	return &ServiceProxy{
		byCtx:          make(map[string]*httputil.ReverseProxy),
		tunnelPatterns: pats,
		tunnels:        newTunnelManager(),
	}
}

// shouldTunnel reports whether a service is configured to use a port-forward
// tunnel. A pattern containing "/" matches "namespace/service", else "service".
func (p *ServiceProxy) shouldTunnel(ns, svc string) bool {
	for _, pat := range p.tunnelPatterns {
		target := svc
		if strings.Contains(pat, "/") {
			target = ns + "/" + svc
		}
		if ok, _ := path.Match(pat, target); ok {
			return true
		}
	}
	return false
}

// ServeHTTP handles /proxy/{context}/{namespace}/{service}/{port}/{rest...}.
func (p *ServiceProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, ProxyPrefix)
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		http.Error(w, "expected /proxy/{context}/{namespace}/{service}/{port}/...", http.StatusBadRequest)
		return
	}
	ctxName := unescape(parts[0])
	namespace := unescape(parts[1])
	service := unescape(parts[2])
	port, err := strconv.Atoi(parts[3])
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	subPath := ""
	if len(parts) == 5 {
		subPath = parts[4]
	}

	// The browser-facing prefix for this service, so redirects/HTML that the
	// upstream emits relative to its own root get rewritten to stay in-proxy.
	// (k8s names are DNS-safe, so the decoded parts equal their escaped form.)
	prefix := ProxyPrefix + parts[0] + "/" + parts[1] + "/" + parts[2] + "/" + parts[3]

	// Services configured for tunneling go through a port-forward (all HTTP
	// methods work without Admin RBAC); everything else uses the API server.
	if p.shouldTunnel(namespace, service) {
		p.serveTunnel(w, r, ctxName, namespace, service, port, subPath, prefix)
		return
	}

	rp, err := p.proxyFor(ctxName)
	if err != nil {
		http.Error(w, "cannot reach API server for context "+ctxName+": "+err.Error(), http.StatusBadGateway)
		return
	}

	// Rewrite the request path to the API-server service-proxy subresource:
	//   /api/v1/namespaces/{ns}/services/[https:]{svc}:{port}/proxy/{sub}
	svcRef := fmt.Sprintf("%s:%d", service, port)
	if httpsProxyPorts[port] {
		svcRef = "https:" + svcRef
	}
	apiBase := fmt.Sprintf("/api/v1/namespaces/%s/services/%s/proxy", namespace, svcRef)
	r.URL.Path = apiBase + "/" + subPath
	r.URL.RawPath = ""
	r = r.WithContext(context.WithValue(r.Context(), metaKey, proxyMeta{prefix: prefix, apiBase: apiBase}))
	rp.ServeHTTP(w, r)
}

// serveTunnel proxies a request to the service via a port-forward tunnel. The
// upstream is the app directly (no API-server rewriting), so rewriteResponse
// runs in prepend-prefix mode (apiBase left empty).
func (p *ServiceProxy) serveTunnel(w http.ResponseWriter, r *http.Request, ctxName, ns, svc string, port int, subPath, prefix string) {
	localPort, err := p.tunnels.endpoint(ctxName, ns, svc, port)
	if err != nil {
		http.Error(w, "cannot establish port-forward to "+ns+"/"+svc+": "+err.Error(), http.StatusBadGateway)
		return
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", localPort)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = "/" + subPath
		req.URL.RawPath = ""
		req.Header.Del("Accept-Encoding") // so we can rewrite bodies
		req.Host = target.Host
	}
	rp.ModifyResponse = rewriteResponse
	rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, e error) {
		p.tunnels.evict(tunnelKey(ctxName, ns, svc, port)) // tunnel may be dead; rebuild next time
		http.Error(rw, "tunnel error: "+e.Error(), http.StatusBadGateway)
	}
	// Empty apiBase => rewriteResponse uses prepend-prefix HTML mode.
	r = r.WithContext(context.WithValue(r.Context(), metaKey, proxyMeta{prefix: prefix, apiBase: ""}))
	rp.ServeHTTP(w, r)
}

// proxyFor returns (building and caching if needed) a reverse proxy targeting
// the given context's API server, with credentials baked in.
func (p *ServiceProxy) proxyFor(context string) (*httputil.ReverseProxy, error) {
	p.mu.Lock()
	if rp, ok := p.byCtx[context]; ok {
		p.mu.Unlock()
		return rp, nil
	}
	p.mu.Unlock()

	cfg, err := resolveContext(context)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(cfg.server)
	if err != nil {
		return nil, fmt.Errorf("bad server URL for context %s: %w", context, err)
	}

	transport, err := cfg.transport()
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(target) // joins target.Path (e.g. Devtron proxy base) + our path
	rp.Transport = transport
	orig := rp.Director
	rp.Director = func(req *http.Request) {
		// target.Path (server base, e.g. /orchestrator/k8s/proxy/cluster/X) is
		// prepended to our /api/v1/... path by joinURLPath inside orig.
		orig(req)
		if cfg.token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.token)
		}
		// Do NOT forward X-Forwarded-For — gateways reject loopback/private IPs.
		req.Header["X-Forwarded-For"] = nil
		// Ask for uncompressed responses so we can rewrite HTML bodies.
		req.Header.Del("Accept-Encoding")
		req.Host = target.Host
	}
	rp.ModifyResponse = rewriteResponse
	rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, e error) {
		http.Error(rw, "proxy error: "+e.Error(), http.StatusBadGateway)
	}

	p.mu.Lock()
	p.byCtx[context] = rp
	p.mu.Unlock()
	return rp, nil
}

func unescape(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

// rewriteResponse makes upstream apps that assume they're served at their own
// root (redirects, absolute-path assets, Grafana's appSubUrl, cookie Path) work
// when served under our proxy prefix. Best-effort: HTML + redirects + cookies.
//
// The API server already rewrites absolute links in proxied HTML/redirects to
// start with its own service-proxy base (apiBase); we translate apiBase back to
// our browser-facing prefix. Grafana's appSubUrl (in bootData JSON) is not
// touched by the API server, so we prepend the prefix to it separately.
func rewriteResponse(resp *http.Response) error {
	meta, _ := resp.Request.Context().Value(metaKey).(proxyMeta)
	if meta.prefix == "" {
		return nil
	}
	prefix, apiBase := meta.prefix, meta.apiBase

	// 1) Redirects.
	if loc := resp.Header.Get("Location"); loc != "" {
		resp.Header.Set("Location", rewriteLocation(loc, prefix, apiBase))
	}

	// 2) Cookie Path so the browser keeps sending them under our prefix.
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		rewritten := make([]string, len(cookies))
		for i, c := range cookies {
			if apiBase != "" {
				c = strings.ReplaceAll(c, apiBase, prefix)
			}
			c = reCookiePath.ReplaceAllStringFunc(c, func(m string) string {
				sub := reCookiePath.FindStringSubmatch(m)
				if strings.HasPrefix(sub[2], prefix) {
					return m
				}
				return sub[1] + prefix + sub[2]
			})
			rewritten[i] = c
		}
		resp.Header.Del("Set-Cookie")
		for _, c := range rewritten {
			resp.Header.Add("Set-Cookie", c)
		}
	}

	// 3) HTML body.
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	s := string(body)
	if apiBase != "" {
		// Service-proxy mode: the API server already rewrote absolute links to
		// start with apiBase; swap that base for our prefix.
		s = strings.ReplaceAll(s, apiBase, prefix)
	} else {
		// Tunnel mode: upstream is the app directly; prepend the prefix to
		// absolute-path attributes (skip ones already under the prefix).
		s = reAttrAbsPath.ReplaceAllStringFunc(s, func(m string) string {
			sub := reAttrAbsPath.FindStringSubmatch(m)
			if strings.HasPrefix(sub[2], prefix) {
				return m
			}
			return sub[1] + prefix + sub[2]
		})
	}
	s = reAppSubURL.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAppSubURL.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], prefix) {
			return m
		}
		return sub[1] + prefix + sub[2]
	})

	resp.Body = io.NopCloser(strings.NewReader(s))
	resp.ContentLength = int64(len(s))
	resp.Header.Set("Content-Length", strconv.Itoa(len(s)))
	resp.Header.Del("Content-Encoding") // body is now plain text
	return nil
}

// rewriteLocation maps a redirect Location back into our proxy prefix.
func rewriteLocation(loc, prefix, apiBase string) string {
	// Already an API-server-relative path -> swap its base for ours.
	if apiBase != "" && strings.Contains(loc, apiBase) {
		return strings.ReplaceAll(loc, apiBase, prefix)
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	// Same-path already under our prefix: leave it.
	if strings.HasPrefix(u.Path, prefix) {
		return loc
	}
	// Absolute (e.g. Grafana's root_url on another host) or root-relative:
	// keep just the path+query and move it under our prefix.
	if strings.HasPrefix(u.Path, "/") {
		nl := prefix + u.Path
		if u.RawQuery != "" {
			nl += "?" + u.RawQuery
		}
		return nl
	}
	return loc
}

// restConfig is the minimal connection info for one context.
type restConfig struct {
	server     string
	token      string
	caData     []byte
	clientCert []byte
	clientKey  []byte
	insecure   bool
}

func (c *restConfig) transport() (*http.Transport, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: c.insecure} //nolint:gosec // honors kubeconfig insecure-skip-tls-verify
	if len(c.caData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(c.caData) {
			return nil, fmt.Errorf("failed to parse certificate-authority-data")
		}
		tlsCfg.RootCAs = pool
	}
	if len(c.clientCert) > 0 && len(c.clientKey) > 0 {
		cert, err := tls.X509KeyPair(c.clientCert, c.clientKey)
		if err != nil {
			return nil, fmt.Errorf("bad client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Transport{TLSClientConfig: tlsCfg}, nil
}

// kubeconfig JSON shapes we care about.
type kubeconfigFile struct {
	Clusters []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server                   string `json:"server"`
			CertificateAuthorityData string `json:"certificate-authority-data"`
			CertificateAuthority     string `json:"certificate-authority"`
			InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify"`
		} `json:"cluster"`
	} `json:"clusters"`
	Contexts []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster string `json:"cluster"`
			User    string `json:"user"`
		} `json:"context"`
	} `json:"contexts"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Token                 string `json:"token"`
			ClientCertificateData string `json:"client-certificate-data"`
			ClientKeyData         string `json:"client-key-data"`
			ClientCertificate     string `json:"client-certificate"`
			ClientKey             string `json:"client-key"`
		} `json:"user"`
	} `json:"users"`
}

// resolveContext extracts server URL + credentials for a context from the
// (fully resolved) kubeconfig.
func resolveContext(ctxName string) (*restConfig, error) {
	out, err := runKubectl("config", "view", "--raw", "-o", "json")
	if err != nil {
		return nil, err
	}
	var kc kubeconfigFile
	if err := json.Unmarshal(out, &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}

	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			clusterName, userName = c.Context.Cluster, c.Context.User
			break
		}
	}
	if clusterName == "" {
		return nil, fmt.Errorf("context %q not found in kubeconfig", ctxName)
	}

	cfg := &restConfig{}
	for _, c := range kc.Clusters {
		if c.Name == clusterName {
			cfg.server = c.Cluster.Server
			cfg.insecure = c.Cluster.InsecureSkipTLSVerify
			cfg.caData = decodeOrRead(c.Cluster.CertificateAuthorityData, c.Cluster.CertificateAuthority)
			break
		}
	}
	if cfg.server == "" {
		return nil, fmt.Errorf("cluster %q for context %q has no server URL", clusterName, ctxName)
	}
	for _, u := range kc.Users {
		if u.Name == userName {
			cfg.token = u.User.Token
			cfg.clientCert = decodeOrRead(u.User.ClientCertificateData, u.User.ClientCertificate)
			cfg.clientKey = decodeOrRead(u.User.ClientKeyData, u.User.ClientKey)
			break
		}
	}
	if cfg.token == "" && len(cfg.clientCert) == 0 {
		return nil, fmt.Errorf("context %q has no bearer token or client cert; exec-plugin auth is not supported", ctxName)
	}
	return cfg, nil
}

// decodeOrRead returns the base64-decoded inline data, or the contents of the
// file path, whichever is present.
func decodeOrRead(b64Data, filePath string) []byte {
	if b64Data != "" {
		if b, err := base64.StdEncoding.DecodeString(b64Data); err == nil {
			return b
		}
	}
	if filePath != "" {
		if b, err := os.ReadFile(filePath); err == nil {
			return b
		}
	}
	return nil
}
