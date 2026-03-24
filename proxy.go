package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

// Proxy routes incoming HTTP requests to the correct SSH tunnel based on subdomain.
type Proxy struct {
	domain   string
	registry *Registry
}

func NewProxy(domain string, registry *Registry) *Proxy {
	return &Proxy{domain: domain, registry: registry}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Caddy on_demand_tls check: is this subdomain active?
	if r.URL.Path == "/tunnel-check" {
		p.handleTunnelCheck(w, r)
		return
	}

	subdomain := p.extractSubdomain(r.Host)
	if subdomain == "" {
		http.Error(w, "No tunnel specified. Use <id>."+p.domain, http.StatusBadRequest)
		return
	}

	tunnel := p.registry.Get(subdomain)
	if tunnel == nil {
		http.Error(w, "Tunnel not found or inactive", http.StatusNotFound)
		return
	}

	if isWebSocketUpgrade(r) {
		p.handleWebSocket(w, r, tunnel)
		return
	}

	p.handleHTTP(w, r, tunnel)
}

// handleTunnelCheck responds to Caddy's on_demand_tls "ask" request.
// Caddy sends GET /tunnel-check?domain=<subdomain>.tunnel.example.com
// Return 200 if the tunnel is active, 404 otherwise.
func (p *Proxy) handleTunnelCheck(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	subdomain := p.extractSubdomain(domain)
	if subdomain != "" && p.registry.Get(subdomain) != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "no active tunnel", http.StatusNotFound)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request, tunnel *Tunnel) {
	ch, err := tunnel.Dial(r.RemoteAddr)
	if err != nil {
		log.Printf("Dial tunnel %s: %v", tunnel.Subdomain, err)
		http.Error(w, "Tunnel unavailable", http.StatusBadGateway)
		return
	}
	defer ch.Close()

	// Forward the HTTP request through the SSH channel
	if err := r.Write(ch); err != nil {
		log.Printf("Write to tunnel %s: %v", tunnel.Subdomain, err)
		http.Error(w, "Failed to reach tunnel", http.StatusBadGateway)
		return
	}

	// Read the response from the local service
	resp, err := http.ReadResponse(bufio.NewReader(ch), r)
	if err != nil {
		log.Printf("Read from tunnel %s: %v", tunnel.Subdomain, err)
		http.Error(w, "Bad response from tunnel", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the body with flushing (supports SSE / chunked streaming)
	if flusher, ok := w.(http.Flusher); ok {
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}

func (p *Proxy) handleWebSocket(w http.ResponseWriter, r *http.Request, tunnel *Tunnel) {
	ch, err := tunnel.Dial(r.RemoteAddr)
	if err != nil {
		log.Printf("Dial tunnel %s (ws): %v", tunnel.Subdomain, err)
		http.Error(w, "Tunnel unavailable", http.StatusBadGateway)
		return
	}
	defer ch.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Hijack failed: %v", err)
		return
	}
	defer clientConn.Close()

	// Forward the upgrade request to the local service
	if err := r.Write(ch); err != nil {
		return
	}

	// Bidirectional byte copy (upgrade response + websocket frames)
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(ch, clientBuf)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(clientConn, ch)
		errc <- err
	}()
	<-errc
}

func (p *Proxy) extractSubdomain(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	suffix := "." + p.domain
	if strings.HasSuffix(host, suffix) {
		return strings.TrimSuffix(host, suffix)
	}
	return ""
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
