package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	domain := flag.String("domain", envOr("TUNNEL_DOMAIN", ""), "Base domain for tunnels (required)")
	sshAddr := flag.String("ssh-addr", envOr("TUNNEL_SSH_ADDR", ":3015"), "SSH listen address")
	httpAddr := flag.String("http-addr", envOr("TUNNEL_HTTP_ADDR", ":8080"), "HTTP listen address")
	hostKeyPath := flag.String("host-key", envOr("TUNNEL_HOST_KEY", "host_key"), "SSH host key (auto-generated if missing)")
	authKeysPath := flag.String("authorized-keys", envOr("TUNNEL_AUTHORIZED_KEYS", ""), "Path to authorized_keys file")
	password := flag.String("password", envOr("TUNNEL_PASSWORD", ""), "Password for SSH auth")
	flag.Parse()

	if *domain == "" {
		fmt.Fprintln(os.Stderr, "Error: -domain (or TUNNEL_DOMAIN) is required")
		flag.Usage()
		os.Exit(1)
	}

	if *password == "" && *authKeysPath == "" {
		fmt.Fprintln(os.Stderr, "Error: provide -password or -authorized-keys (or both)")
		flag.Usage()
		os.Exit(1)
	}

	// Auto-generate host key if it doesn't exist
	if _, err := os.Stat(*hostKeyPath); os.IsNotExist(err) {
		log.Printf("Generating SSH host key at %s", *hostKeyPath)
		if err := generateHostKey(*hostKeyPath); err != nil {
			log.Fatalf("Failed to generate host key: %v", err)
		}
	}

	registry := NewRegistry()

	sshServer, err := NewSSHServer(*hostKeyPath, *authKeysPath, *password, *domain, registry)
	if err != nil {
		log.Fatalf("Failed to start SSH server: %v", err)
	}

	proxy := NewProxy(*domain, registry)

	// Start SSH server
	sshLn, err := net.Listen("tcp", *sshAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", *sshAddr, err)
	}
	go func() {
		log.Printf("SSH server listening on %s", *sshAddr)
		for {
			conn, err := sshLn.Accept()
			if err != nil {
				log.Printf("SSH accept error: %v", err)
				continue
			}
			go sshServer.HandleConn(conn)
		}
	}()

	// Start HTTP server
	go func() {
		log.Printf("HTTP proxy listening on %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, proxy); err != nil {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	log.Printf("Tunnels will be available at https://<id>.%s", *domain)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// generateHostKey creates an Ed25519 SSH host key in PEM format.
func generateHostKey(path string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
}
