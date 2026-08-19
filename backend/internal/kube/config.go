package kube

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ServiceConfig is the curated, ConfigMap-driven list of UI-serving services to
// show, keyed by namespace. When loaded, ListServices returns only matching
// services for configured namespaces (uncurated namespaces fall back to listing
// everything). Loaded from JSON at startup.
type ServiceConfig struct {
	// RequireEndpoints drops services that have no endpoint IPs (no ready pods).
	// Defaults to true when omitted.
	RequireEndpoints *bool `json:"requireEndpoints"`
	// Namespaces maps a namespace to its curated entries, in display order.
	Namespaces map[string][]CuratedEntry `json:"namespaces"`
}

// CuratedEntry selects one logical service by label (+ optional name filters)
// and optionally pins the port to expose.
type CuratedEntry struct {
	Name string `json:"name"` // friendly/logical name shown in the UI
	// LabelKey defaults to "app.kubernetes.io/name" when empty.
	LabelKey   string `json:"labelKey"`
	LabelValue string `json:"labelValue"`
	// NameContains, if set, requires the service name to contain this substring
	// (disambiguates services that share a label, e.g. alloy vs alloy-cluster).
	NameContains string `json:"nameContains"`
	// ExcludeNameContains skips services whose name contains any of these
	// (e.g. drop "headless"/"memberlist" variants).
	ExcludeNameContains []string `json:"excludeNameContains"`
	// Port pins the port to expose (0 = use the service's discovered ports).
	Port int `json:"port"`
}

// LoadServiceConfig reads and parses the JSON config at path.
func LoadServiceConfig(path string) (*ServiceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ServiceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing service config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *ServiceConfig) requireEndpoints() bool {
	return c.RequireEndpoints == nil || *c.RequireEndpoints
}

// matches reports whether a service satisfies the entry's selectors.
func (e CuratedEntry) matches(svc *Service) bool {
	key := e.LabelKey
	if key == "" {
		key = "app.kubernetes.io/name"
	}
	if svc.labels[key] != e.LabelValue {
		return false
	}
	if e.NameContains != "" && !strings.Contains(svc.Name, e.NameContains) {
		return false
	}
	for _, ex := range e.ExcludeNameContains {
		if ex != "" && strings.Contains(svc.Name, ex) {
			return false
		}
	}
	return true
}

// curate filters and orders services for a namespace per the config. ready, if
// non-nil, is the set of service names that have endpoint IPs.
func (c *ServiceConfig) curate(namespace string, all []Service, ready map[string]bool) []Service {
	entries := c.Namespaces[namespace]
	result := make([]Service, 0, len(entries))
	seen := make(map[string]bool)
	for _, e := range entries {
		for i := range all {
			s := all[i]
			if !e.matches(&s) {
				continue
			}
			if ready != nil && !ready[s.Name] {
				continue
			}
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			s.DisplayName = e.Name
			if e.Port != 0 {
				s.Ports = pinPort(s.Ports, e.Port)
			}
			s.labels = nil // don't leak labels to the API response
			result = append(result, s)
		}
	}
	return result
}

// pinPort restricts a service's exposed ports to the pinned one, synthesizing an
// entry if the service doesn't declare it.
func pinPort(ports []ServicePort, port int) []ServicePort {
	for _, p := range ports {
		if p.Port == port {
			return []ServicePort{p}
		}
	}
	return []ServicePort{{Port: port, Protocol: "TCP"}}
}
