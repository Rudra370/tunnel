package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Tunnel represents an active SSH tunnel mapped to a public subdomain.
type Tunnel struct {
	Subdomain string
	SSHConn   *ssh.ServerConn
	BindAddr  string // address from the tcpip-forward request
	BindPort  uint32 // port we told the client we bound (used in forwarded-tcpip payload)
}

// forwardedTCPPayload is the SSH wire format for opening a forwarded-tcpip channel.
type forwardedTCPPayload struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

// Dial opens a forwarded-tcpip channel to the client's local service.
// The SSH client matches (Addr, Port) to determine which -R rule applies.
func (t *Tunnel) Dial(remoteAddr string) (ssh.Channel, error) {
	originAddr, originPort := parseAddr(remoteAddr)

	payload := forwardedTCPPayload{
		Addr:       t.BindAddr,
		Port:       t.BindPort,
		OriginAddr: originAddr,
		OriginPort: originPort,
	}

	ch, reqs, err := t.SSHConn.OpenChannel("forwarded-tcpip", ssh.Marshal(&payload))
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	go ssh.DiscardRequests(reqs)
	return ch, nil
}

func parseAddr(addr string) (string, uint32) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, uint32(port)
}

// Registry maps subdomains to active tunnels (thread-safe).
type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
}

func NewRegistry() *Registry {
	return &Registry{tunnels: make(map[string]*Tunnel)}
}

func (r *Registry) Add(subdomain string, t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunnels[subdomain] = t
}

func (r *Registry) Get(subdomain string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[subdomain]
}

func (r *Registry) Remove(subdomain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tunnels, subdomain)
}

func generateSubdomain() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
