package kube

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ForwardBindAddress is the address kubectl port-forward binds forwarded ports
// to (kubectl --address). Set from FORWARD_BIND_ADDRESS at startup; defaults to
// 127.0.0.1 for local use, should be 0.0.0.0 in-cluster so peers can connect.
var ForwardBindAddress = "127.0.0.1"

// ForwardRequest is the payload to start a new port-forward.
type ForwardRequest struct {
	Context     string `json:"context"`
	Namespace   string `json:"namespace"`
	Service     string `json:"service"`
	RemotePort  int    `json:"remotePort"`
	LocalPort   int    `json:"localPort"` // 0 = let kubectl pick a free port
}

// Forward is a running (or failed) port-forward.
type Forward struct {
	ID         string    `json:"id"`
	Context    string    `json:"context"`
	Namespace  string    `json:"namespace"`
	Service    string    `json:"service"`
	RemotePort int       `json:"remotePort"`
	LocalPort  int       `json:"localPort"`
	Status     string    `json:"status"` // "running" | "error" | "stopped"
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`

	cmd  *exec.Cmd `json:"-"`
	logs []string  `json:"-"`
}

// ForwardManager tracks running kubectl port-forward processes.
type ForwardManager struct {
	mu       sync.Mutex
	forwards map[string]*Forward
	seq      int
}

func NewForwardManager() *ForwardManager {
	return &ForwardManager{forwards: make(map[string]*Forward)}
}

// Start spawns `kubectl port-forward` and watches its output to detect the
// chosen local port and any failure.
func (m *ForwardManager) Start(req ForwardRequest) (*Forward, error) {
	if req.Context == "" || req.Namespace == "" || req.Service == "" || req.RemotePort == 0 {
		return nil, fmt.Errorf("context, namespace, service and remotePort are required")
	}

	m.mu.Lock()
	m.seq++
	id := strconv.Itoa(m.seq)
	m.mu.Unlock()

	// "localPort:remotePort" — an empty local part tells kubectl to pick a free port.
	portArg := fmt.Sprintf("%d:%d", req.LocalPort, req.RemotePort)
	if req.LocalPort == 0 {
		portArg = fmt.Sprintf(":%d", req.RemotePort)
	}

	cmd := exec.Command("kubectl",
		"--context", req.Context,
		"-n", req.Namespace,
		"port-forward",
		"--address", ForwardBindAddress,
		"svc/"+req.Service,
		portArg,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	fwd := &Forward{
		ID:         id,
		Context:    req.Context,
		Namespace:  req.Namespace,
		Service:    req.Service,
		RemotePort: req.RemotePort,
		LocalPort:  req.LocalPort,
		Status:     "starting",
		StartedAt:  time.Now(),
		cmd:        cmd,
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.forwards[id] = fwd
	m.mu.Unlock()

	// kubectl prints "Forwarding from 127.0.0.1:PORT -> remote" on success.
	ready := make(chan struct{})
	var once sync.Once
	go m.scan(fwd, stdout, ready, &once)
	go m.scan(fwd, stderr, ready, &once)

	// Reap the process and record its exit.
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		if fwd.Status == "starting" || fwd.Status == "running" {
			if err != nil {
				fwd.Status = "error"
				if fwd.Error == "" {
					fwd.Error = err.Error()
				}
			} else {
				fwd.Status = "stopped"
			}
		}
	}()

	// Wait briefly for kubectl to confirm the forward or fail fast.
	select {
	case <-ready:
	case <-time.After(6 * time.Second):
	}

	// If kubectl never printed a confirmation and hasn't exited, leave the
	// status as "starting" — the /forwards poll will flip it to running/error
	// once kubectl emits output. Don't falsely claim "running".
	m.mu.Lock()
	defer m.mu.Unlock()
	return fwd, nil
}

func (m *ForwardManager) scan(fwd *Forward, r interface{ Read([]byte) (int, error) }, ready chan struct{}, once *sync.Once) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		m.mu.Lock()
		fwd.logs = append(fwd.logs, line)
		if strings.Contains(line, "Forwarding from") {
			if p := parseLocalPort(line); p != 0 {
				fwd.LocalPort = p
			}
			fwd.Status = "running"
			m.mu.Unlock()
			once.Do(func() { close(ready) })
			continue
		}
		if strings.Contains(strings.ToLower(line), "error") || strings.Contains(line, "Unable to listen") {
			fwd.Status = "error"
			fwd.Error = line
			m.mu.Unlock()
			once.Do(func() { close(ready) })
			continue
		}
		m.mu.Unlock()
	}
}

// parseLocalPort extracts the port from "Forwarding from 127.0.0.1:52345 -> 8080".
func parseLocalPort(line string) int {
	idx := strings.Index(line, "Forwarding from")
	if idx < 0 {
		return 0
	}
	rest := line[idx:]
	colon := strings.LastIndex(strings.SplitN(rest, "->", 2)[0], ":")
	if colon < 0 {
		return 0
	}
	portStr := strings.TrimSpace(rest[colon+1:])
	if arrow := strings.Index(portStr, " "); arrow >= 0 {
		portStr = portStr[:arrow]
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return p
}

// Stop kills a running forward and removes it from the manager.
func (m *ForwardManager) Stop(id string) error {
	m.mu.Lock()
	fwd, ok := m.forwards[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("forward %s not found", id)
	}
	delete(m.forwards, id)
	m.mu.Unlock()

	if fwd.cmd != nil && fwd.cmd.Process != nil {
		_ = fwd.cmd.Process.Kill()
	}
	return nil
}

// List returns all tracked forwards.
func (m *ForwardManager) List() []*Forward {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Forward, 0, len(m.forwards))
	for _, f := range m.forwards {
		out = append(out, f)
	}
	return out
}
