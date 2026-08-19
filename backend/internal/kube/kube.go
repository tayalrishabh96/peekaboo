// Package kube wraps the local kubectl binary to read kubeconfig data and
// manage port-forward processes.
package kube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// ConfigPath returns the kubeconfig path (KUBECONFIG env var or ~/.kube/config).
func ConfigPath() string {
	if p := os.Getenv("KUBECONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

// runKubectl executes kubectl with the given args and returns stdout, surfacing
// stderr in the error so failures are debuggable in the UI.
func runKubectl(args ...string) ([]byte, error) {
	cmd := exec.Command("kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("kubectl %v: %s", args, msg)
	}
	return stdout.Bytes(), nil
}

// Context is a kubeconfig context, surfaced to the UI as a "cluster".
type Context struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	Current   bool   `json:"current"`
}

// ListContexts parses `kubectl config view` output.
func ListContexts() ([]Context, error) {
	out, err := runKubectl("config", "view", "-o", "json")
	if err != nil {
		return nil, err
	}
	var cfg struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster   string `json:"cluster"`
				Namespace string `json:"namespace"`
			} `json:"context"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	result := make([]Context, 0, len(cfg.Contexts))
	for _, c := range cfg.Contexts {
		result = append(result, Context{
			Name:      c.Name,
			Cluster:   c.Context.Cluster,
			Namespace: c.Context.Namespace,
			Current:   c.Name == cfg.CurrentContext,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// Namespace is a k8s namespace.
type Namespace struct {
	Name string `json:"name"`
}

// ListNamespaces lists namespaces in the given context.
func ListNamespaces(context string) ([]Namespace, error) {
	out, err := runKubectl("--context", context, "get", "namespaces", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parsing namespaces: %w", err)
	}
	result := make([]Namespace, 0, len(list.Items))
	for _, item := range list.Items {
		result = append(result, Namespace{Name: item.Metadata.Name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// ServicePort describes one port exposed by a service.
type ServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort string `json:"targetPort"`
	Protocol   string `json:"protocol"`
}

// Service is a k8s service with its ports.
type Service struct {
	Name        string        `json:"name"`
	DisplayName string        `json:"displayName,omitempty"` // friendly name from curated config
	Namespace   string        `json:"namespace"`
	Type        string        `json:"type"`
	ClusterIP   string        `json:"clusterIP"`
	Ports       []ServicePort `json:"ports"`

	labels map[string]string // service labels, used for curation (not serialized)
}

// ListServices lists services in the given context/namespace. When cfg is
// non-nil and the namespace is curated, only matching services are returned
// (and, if configured, services without endpoint IPs are dropped).
func ListServices(context, namespace string, cfg *ServiceConfig) ([]Service, error) {
	out, err := runKubectl("--context", context, "-n", namespace, "get", "services", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Type      string `json:"type"`
				ClusterIP string `json:"clusterIP"`
				Ports     []struct {
					Name       string      `json:"name"`
					Port       int         `json:"port"`
					TargetPort interface{} `json:"targetPort"`
					Protocol   string      `json:"protocol"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parsing services: %w", err)
	}
	result := make([]Service, 0, len(list.Items))
	for _, item := range list.Items {
		svc := Service{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Type:      item.Spec.Type,
			ClusterIP: item.Spec.ClusterIP,
			Ports:     []ServicePort{}, // never emit null; some services (e.g. headless) have no ports
			labels:    item.Metadata.Labels,
		}
		for _, p := range item.Spec.Ports {
			svc.Ports = append(svc.Ports, ServicePort{
				Name:       p.Name,
				Port:       p.Port,
				TargetPort: fmt.Sprintf("%v", p.TargetPort),
				Protocol:   p.Protocol,
			})
		}
		result = append(result, svc)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })

	// No curation, or this namespace isn't curated -> return everything.
	if cfg == nil || cfg.Namespaces[namespace] == nil {
		return result, nil
	}

	// Drop services with no endpoint IPs (best-effort; if we can't read
	// endpoints, skip the filter rather than hide everything).
	var ready map[string]bool
	if cfg.requireEndpoints() {
		if r, err := listReadyServiceNames(context, namespace); err == nil {
			ready = r
		}
	}
	return cfg.curate(namespace, result, ready), nil
}

// listReadyServiceNames returns the set of services in the namespace that have
// at least one endpoint address. Uses the core Endpoints API (not
// EndpointSlices, which need discovery.k8s.io access a View token lacks).
func listReadyServiceNames(context, namespace string) (map[string]bool, error) {
	out, err := runKubectl("--context", context, "-n", namespace, "get", "endpoints", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Subsets []struct {
				Addresses []struct {
					IP string `json:"ip"`
				} `json:"addresses"`
			} `json:"subsets"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parsing endpoints: %w", err)
	}
	ready := make(map[string]bool)
	for _, item := range list.Items {
		for _, ss := range item.Subsets {
			if len(ss.Addresses) > 0 {
				ready[item.Metadata.Name] = true
				break
			}
		}
	}
	return ready, nil
}
