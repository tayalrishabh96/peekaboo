package kube

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
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

// ServiceProxy reverse-proxies browser requests to cluster services through the
// selected context's API server.
type ServiceProxy struct {
	mu    sync.Mutex
	byCtx map[string]*httputil.ReverseProxy
}

func NewServiceProxy() *ServiceProxy {
	return &ServiceProxy{byCtx: make(map[string]*httputil.ReverseProxy)}
}

// ServeHTTP handles /proxy/{context}/{namespace}/{service}/{port}/{rest...}.
func (p *ServiceProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, ProxyPrefix)
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		http.Error(w, "expected /proxy/{context}/{namespace}/{service}/{port}/...", http.StatusBadRequest)
		return
	}
	context := unescape(parts[0])
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

	rp, err := p.proxyFor(context)
	if err != nil {
		http.Error(w, "cannot reach API server for context "+context+": "+err.Error(), http.StatusBadGateway)
		return
	}

	// Rewrite the request path to the API-server service-proxy subresource:
	//   /api/v1/namespaces/{ns}/services/[https:]{svc}:{port}/proxy/{sub}
	svcRef := fmt.Sprintf("%s:%d", service, port)
	if httpsProxyPorts[port] {
		svcRef = "https:" + svcRef
	}
	r.URL.Path = fmt.Sprintf("/api/v1/namespaces/%s/services/%s/proxy/%s", namespace, svcRef, subPath)
	r.URL.RawPath = ""
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
		req.Host = target.Host
	}
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
