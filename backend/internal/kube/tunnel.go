package kube

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tunnelIdleTimeout is how long an unused port-forward is kept before reaping.
const tunnelIdleTimeout = 5 * time.Minute

// tunnel is a single `kubectl port-forward` process feeding a local port.
type tunnel struct {
	localPort int
	cmd       *exec.Cmd
	ready     chan struct{}
	err       error
	lastUsed  time.Time
}

// tunnelManager lazily starts, shares and reaps port-forwards, keyed by
// context+namespace+service+port. Used for services that need write methods
// (e.g. Grafana login POST) which the API-server service-proxy path can't do
// without Admin RBAC — a port-forward only needs pods/portforward.
type tunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*tunnel
}

func newTunnelManager() *tunnelManager {
	m := &tunnelManager{tunnels: make(map[string]*tunnel)}
	go m.reapLoop()
	return m
}

func tunnelKey(ctxName, ns, svc string, port int) string {
	return fmt.Sprintf("%s|%s|%s|%d", ctxName, ns, svc, port)
}

// endpoint returns the local port of a live port-forward for the service,
// starting one (and reusing it across callers) as needed.
func (m *tunnelManager) endpoint(ctxName, ns, svc string, port int) (int, error) {
	key := tunnelKey(ctxName, ns, svc, port)

	m.mu.Lock()
	if t, ok := m.tunnels[key]; ok {
		t.lastUsed = time.Now()
		m.mu.Unlock()
		<-t.ready // may already be closed
		if t.err != nil {
			return 0, t.err
		}
		return t.localPort, nil
	}
	t := &tunnel{ready: make(chan struct{}), lastUsed: time.Now()}
	m.tunnels[key] = t
	m.mu.Unlock()

	// Start synchronously (blocks until kubectl reports its port or fails).
	// Concurrent callers wait on t.ready above.
	m.start(key, t, ctxName, ns, svc, port)
	if t.err != nil {
		m.evict(key)
		return 0, t.err
	}
	return t.localPort, nil
}

func (m *tunnelManager) start(key string, t *tunnel, ctxName, ns, svc string, port int) {
	defer close(t.ready)

	cmd := exec.Command("kubectl",
		"--context", ctxName,
		"-n", ns,
		"port-forward",
		"svc/"+svc,
		fmt.Sprintf(":%d", port), // empty local part => kubectl picks a free port
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.err = err
		return
	}
	cmd.Stderr = cmd.Stdout // fold stderr in so failures show up in one stream
	if err := cmd.Start(); err != nil {
		t.err = err
		return
	}
	t.cmd = cmd

	// When the process exits (pod died, network blip), drop the entry so the
	// next request rebuilds it.
	go func() {
		_ = cmd.Wait()
		m.evict(key)
	}()

	// kubectl prints "Forwarding from 127.0.0.1:PORT -> targetPort".
	portCh := make(chan int, 1)
	errCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "Forwarding from") {
				if p := parseLocalPort(line); p != 0 {
					portCh <- p
					return
				}
			}
			if strings.Contains(strings.ToLower(line), "error") || strings.Contains(line, "Unable to listen") {
				errCh <- line
				return
			}
		}
		errCh <- "kubectl port-forward exited before it was ready"
	}()

	select {
	case p := <-portCh:
		t.localPort = p
	case msg := <-errCh:
		m.kill(cmd)
		t.err = fmt.Errorf("%s", msg)
	case <-time.After(15 * time.Second):
		m.kill(cmd)
		t.err = fmt.Errorf("timed out establishing port-forward to %s/%s:%d", ns, svc, port)
	}
}

func (m *tunnelManager) kill(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// evict removes and kills a tunnel. Safe to call repeatedly.
func (m *tunnelManager) evict(key string) {
	m.mu.Lock()
	t, ok := m.tunnels[key]
	delete(m.tunnels, key)
	m.mu.Unlock()
	if ok {
		m.kill(t.cmd)
	}
}

// reapLoop kills tunnels that have been idle longer than tunnelIdleTimeout.
func (m *tunnelManager) reapLoop() {
	for {
		time.Sleep(time.Minute)
		now := time.Now()
		var stale []string
		m.mu.Lock()
		for key, t := range m.tunnels {
			select {
			case <-t.ready: // established (or failed); safe to check age
				if now.Sub(t.lastUsed) > tunnelIdleTimeout {
					stale = append(stale, key)
				}
			default: // still starting; leave it alone
			}
		}
		m.mu.Unlock()
		for _, key := range stale {
			m.evict(key)
		}
	}
}
