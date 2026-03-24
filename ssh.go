package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

type SSHServer struct {
	config   *ssh.ServerConfig
	domain   string
	registry *Registry
}

// Wire format for tcpip-forward request/response (RFC 4254 §7.1).
type tcpipForwardRequest struct {
	BindAddr string
	BindPort uint32
}

type tcpipForwardResponse struct {
	BoundPort uint32
}

func NewSSHServer(hostKeyPath, authKeysPath, password, domain string, registry *Registry) (*SSHServer, error) {
	config := &ssh.ServerConfig{}

	// Public key auth (if authorized_keys provided)
	if authKeysPath != "" {
		keys, err := loadAuthorizedKeys(authKeysPath)
		if err != nil {
			return nil, fmt.Errorf("load authorized keys: %w", err)
		}
		config.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, ak := range keys {
				if bytes.Equal(ak.Marshal(), key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("unauthorized key")
		}
		log.Printf("Public key auth enabled (%d key(s) from %s)", len(keys), authKeysPath)
	}

	// Password auth (if password provided)
	if password != "" {
		config.PasswordCallback = func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == password {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("wrong password")
		}
		log.Printf("Password auth enabled")
	}

	if config.PublicKeyCallback == nil && config.PasswordCallback == nil {
		return nil, fmt.Errorf("no auth method configured")
	}

	// Load host key
	hostKeyBytes, err := os.ReadFile(hostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read host key: %w", err)
	}
	hostKey, err := ssh.ParsePrivateKey(hostKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	config.AddHostKey(hostKey)

	return &SSHServer{config: config, domain: domain, registry: registry}, nil
}

func loadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var keys []ssh.PublicKey
	for len(data) > 0 {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			break
		}
		keys = append(keys, key)
		data = rest
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid keys found in %s", path)
	}
	return keys, nil
}

// HandleConn processes a single SSH connection: authenticates, handles
// tcpip-forward requests, and manages the tunnel lifecycle.
func (s *SSHServer) HandleConn(netConn net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(netConn, s.config)
	if err != nil {
		log.Printf("SSH handshake failed (%s): %v", netConn.RemoteAddr(), err)
		return
	}
	defer sshConn.Close()

	log.Printf("Authenticated: %s (user: %s)", sshConn.RemoteAddr(), sshConn.User())

	// Track tunnels for cleanup on disconnect
	var tunnelSubs []string
	done := make(chan struct{})

	defer func() {
		close(done)
		for _, sub := range tunnelSubs {
			s.registry.Remove(sub)
			log.Printf("Tunnel closed: %s.%s", sub, s.domain)
		}
	}()

	// Coordinate URL display between forward-request handler and session channel
	urlChan := make(chan string, 10)
	sessionChan := make(chan ssh.Channel, 1)

	go s.displayURLs(urlChan, sessionChan, done)
	go s.handleChannels(chans, sessionChan)

	// Main loop: process global requests (tcpip-forward, keepalive, etc.)
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			sub := s.handleForward(sshConn, req)
			if sub != "" {
				tunnelSubs = append(tunnelSubs, sub)
				url := fmt.Sprintf("https://%s.%s", sub, s.domain)
				select {
				case urlChan <- url:
				default:
				}
			}

		case "cancel-tcpip-forward":
			if req.WantReply {
				req.Reply(true, nil)
			}

		case "keepalive@openssh.com":
			if req.WantReply {
				req.Reply(true, nil)
			}

		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// displayURLs waits for both a session channel and tunnel URLs, then prints them.
func (s *SSHServer) displayURLs(urlChan <-chan string, sessionChan <-chan ssh.Channel, done <-chan struct{}) {
	var session ssh.Channel
	var urls []string

	for {
		select {
		case u := <-urlChan:
			urls = append(urls, u)
			if session != nil {
				fmt.Fprintf(session, "  Tunnel: %s\r\n", u)
			}
		case s := <-sessionChan:
			session = s
			if len(urls) > 0 {
				fmt.Fprintf(session, "\r\n")
				for _, u := range urls {
					fmt.Fprintf(session, "  Tunnel: %s\r\n", u)
				}
				fmt.Fprintf(session, "\r\n  Press Ctrl+C to close.\r\n\r\n")
			}
		case <-done:
			return
		}
	}
}

// handleChannels accepts SSH session channels (needed for the client to stay connected
// and for us to print the tunnel URL).
func (s *SSHServer) handleChannels(chans <-chan ssh.NewChannel, sessionChan chan<- ssh.Channel) {
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}

		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}

		select {
		case sessionChan <- ch:
		default:
		}

		go func(reqs <-chan *ssh.Request) {
			for req := range reqs {
				switch req.Type {
				case "shell", "pty-req", "env", "exec":
					if req.WantReply {
						req.Reply(true, nil)
					}
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}(chReqs)
	}
}

// handleForward processes a tcpip-forward request: generates a subdomain,
// registers the tunnel, and replies with the bound port.
// If the SSH username is a valid subdomain and available, it's used as the subdomain.
func (s *SSHServer) handleForward(conn *ssh.ServerConn, req *ssh.Request) string {
	var fwdReq tcpipForwardRequest
	if err := ssh.Unmarshal(req.Payload, &fwdReq); err != nil {
		log.Printf("Bad tcpip-forward payload: %v", err)
		if req.WantReply {
			req.Reply(false, nil)
		}
		return ""
	}

	// Use SSH username as subdomain if valid and available, otherwise random
	requested := strings.ToLower(conn.User())
	var sub string
	if isValidSubdomain(requested) && s.registry.Get(requested) == nil {
		sub = requested
	} else {
		sub = s.generateUniqueSubdomain()
	}

	tunnel := &Tunnel{
		Subdomain: sub,
		SSHConn:   conn,
		BindAddr:  fwdReq.BindAddr,
		BindPort:  80,
	}
	s.registry.Add(sub, tunnel)

	log.Printf("Tunnel created: https://%s.%s → %s:%d", sub, s.domain, fwdReq.BindAddr, fwdReq.BindPort)

	if req.WantReply {
		req.Reply(true, ssh.Marshal(&tcpipForwardResponse{BoundPort: 80}))
	}
	return sub
}

func (s *SSHServer) generateUniqueSubdomain() string {
	for {
		sub := generateSubdomain()
		if s.registry.Get(sub) == nil {
			return sub
		}
	}
}
