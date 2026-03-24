package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// channelConn wraps an ssh.Channel to satisfy net.Conn.
type channelConn struct {
	ssh.Channel
}

func (c *channelConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *channelConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *channelConn) SetDeadline(t time.Time) error      { return nil }
func (c *channelConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *channelConn) SetWriteDeadline(t time.Time) error { return nil }

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
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = req.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				ch, err := tunnel.Dial(r.RemoteAddr)
				if err != nil {
					return nil, err
				}
				return &channelConn{ch}, nil
			},
			DisableKeepAlives: true,
		},
		FlushInterval: -1, // flush immediately for SSE/streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error for %s: %v", tunnel.Subdomain, err)
			http.Error(w, "Tunnel error", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
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
