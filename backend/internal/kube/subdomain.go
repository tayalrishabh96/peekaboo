package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// SubdomainProxy serves a target service under its own subdomain
// (‹slug›.‹baseDomain›) via a port-forward tunnel, preserving the request path
// verbatim. Because the browser origin's path equals the app's native path,
// apps that assume a fixed base path (e.g. Grafana at /grafana) work without any
// response rewriting — only absolute redirect hosts are pulled back onto the
// slug origin.
type SubdomainProxy struct {
	baseDomain string
	tunnels    *tunnelManager

	mu     sync.Mutex
	bySlug map[string]proxyTarget
}

type proxyTarget struct {
	context   string
	namespace string
	service   string
	port      int
}

func NewSubdomainProxy(baseDomain string) *SubdomainProxy {
	return &SubdomainProxy{
		baseDomain: strings.ToLower(baseDomain),
		tunnels:    newTunnelManager(),
		bySlug:     make(map[string]proxyTarget),
	}
}

// Register returns the stable slug subdomain for a target, creating the mapping
// if needed. The slug is deterministic, so repeated calls are idempotent.
func (s *SubdomainProxy) Register(displayName, context, namespace, service string, port int) string {
	slug := makeSlug(displayName, context, namespace, service, port)
	s.mu.Lock()
	s.bySlug[slug] = proxyTarget{context, namespace, service, port}
	s.mu.Unlock()
	return slug
}

// URLFor returns the full https URL for a target's slug subdomain.
func (s *SubdomainProxy) URLFor(displayName, context, namespace, service string, port int) string {
	slug := s.Register(displayName, context, namespace, service, port)
	return "https://" + slug + "." + s.baseDomain + "/"
}

// MatchHost reports whether host is a ‹slug›.‹baseDomain› and returns the slug.
func (s *SubdomainProxy) MatchHost(host string) (string, bool) {
	host = hostOnly(host)
	suffix := "." + s.baseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	slug := strings.TrimSuffix(host, suffix)
	if slug == "" || strings.Contains(slug, ".") { // must be a single label (not the apex, not deeper)
		return "", false
	}
	return slug, true
}

func (s *SubdomainProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.MatchHost(r.Host)
	if !ok {
		http.Error(w, "not a service subdomain", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	t, found := s.bySlug[slug]
	s.mu.Unlock()
	if !found {
		http.Error(w, "unknown or expired link — open this service again from the peekaboo UI", http.StatusNotFound)
		return
	}

	localPort, err := s.tunnels.endpoint(t.context, t.namespace, t.service, t.port)
	if err != nil {
		http.Error(w, "cannot establish tunnel to "+t.namespace+"/"+t.service+": "+err.Error(), http.StatusBadGateway)
		return
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", localPort)}
	reqHost := hostOnly(r.Host)
	rp := httputil.NewSingleHostReverseProxy(target) // default Director preserves the path verbatim
	rp.ModifyResponse = func(resp *http.Response) error {
		// Keep absolute redirects on this origin. The app's root_url points at
		// its real external host; rewrite such Location values to a same-origin
		// path so the browser stays on the slug subdomain.
		if loc := resp.Header.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil && u.Host != "" && u.Host != reqHost {
				np := u.EscapedPath()
				if np == "" {
					np = "/"
				}
				if u.RawQuery != "" {
					np += "?" + u.RawQuery
				}
				resp.Header.Set("Location", np)
			}
		}
		return nil
	}
	rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, e error) {
		s.tunnels.evict(tunnelKey(t.context, t.namespace, t.service, t.port))
		http.Error(rw, "tunnel error: "+e.Error(), http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

// makeSlug builds a DNS-label slug: ‹sanitized-name›-‹6-hex hash of target›.
func makeSlug(displayName, context, namespace, service string, port int) string {
	sum := sha256.Sum256([]byte(context + "|" + namespace + "|" + service + "|" + strconv.Itoa(port)))
	short := hex.EncodeToString(sum[:])[:6]
	name := sanitizeLabel(displayName)
	if name == "" {
		name = sanitizeLabel(service)
	}
	if name == "" {
		name = "svc"
	}
	// Keep within the 63-char DNS label limit, always preserving the hash.
	const maxName = 63 - 1 - 6
	if len(name) > maxName {
		name = strings.Trim(name[:maxName], "-")
	}
	return name + "-" + short
}

// sanitizeLabel lowercases and reduces to [a-z0-9-], trimming stray dashes.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// hostOnly strips any :port from a Host header value.
func hostOnly(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return strings.ToLower(host[:i])
	}
	return strings.ToLower(host)
}
