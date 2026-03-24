# tunneld

A self-hosted SSH tunnel server. Expose your local services to the internet through public HTTPS URLs — like ngrok/Pinggy, but on your own server with no time limits.

```
ssh -p 3017 t@tunnel.example.com -R 0:localhost:3000
```
```
  Tunnel: https://a3f2b1c8.tunnel.example.com

  Press Ctrl+C to close.
```

## How it works

```
┌──────────┐       HTTPS        ┌───────────────┐      SSH Tunnel      ┌──────────────┐
│ Internet │  ───────────────►  │  Your Server  │  ◄────────────────  │ Your Machine │
│  User    │                    │               │   (outbound from    │              │
└──────────┘                    │  Caddy (TLS)  │    your machine)    │  localhost   │
                                │      ↓        │                     │    :3000     │
                                │  tunneld      │                     └──────────────┘
                                │  (HTTP + SSH) │
                                └───────────────┘
```

1. You run `ssh -R` from your machine → opens an outbound connection to your server (works through NAT/firewalls)
2. `tunneld` assigns a random subdomain and registers the tunnel
3. When someone visits `https://<id>.tunnel.example.com`, Caddy terminates TLS and forwards to `tunneld`
4. `tunneld` routes the request through the SSH tunnel back to your local service
5. Response travels the reverse path

No custom client needed — just standard `ssh`.

## Features

- **Password and/or SSH key authentication** — only you can create tunnels
- **Random subdomains** — each tunnel gets a unique `<8-char-hex>.tunnel.example.com`
- **HTTP + WebSocket** support
- **Streaming/SSE** support with response flushing
- **Multiple tunnels** per connection (`-R` can be repeated)
- **Auto-generated host key** — created on first start if missing
- **Docker ready** — single `docker compose up -d`
- **Tiny footprint** — single Go binary, ~10MB Docker image

## Prerequisites

- A server with a public IP (VPS, bare metal, etc.)
- A domain you control
- [Caddy](https://caddyserver.com/) (or any reverse proxy that handles TLS)
- [Docker](https://docs.docker.com/get-docker/) and Docker Compose (for containerized deployment)

## Setup

### Step 1: DNS

Add a **wildcard A record** pointing to your server:

```
*.tunnel.example.com  →  YOUR_SERVER_IP
```

Where to do this depends on your DNS provider (Cloudflare, Namecheap, Route53, etc.). Create an A record with:
- **Name:** `*.tunnel` (if your domain is `example.com`)
- **Value:** your server's public IP
- **TTL:** Auto or 300

### Step 2: Caddy

Add this block to your Caddyfile:

```caddyfile
*.tunnel.example.com {
    reverse_proxy localhost:3018
}
```

Then reload Caddy:

```bash
sudo systemctl reload caddy
```

Caddy will automatically obtain a wildcard TLS certificate via Let's Encrypt. For wildcard certs, Caddy needs a DNS challenge. If you haven't set this up before, you'll need the [Caddy DNS plugin](https://caddyserver.com/docs/modules/) for your provider. Example for Cloudflare:

```caddyfile
*.tunnel.example.com {
    tls {
        dns cloudflare {env.CF_API_TOKEN}
    }
    reverse_proxy localhost:3018
}
```

### Step 3: Deploy tunneld

Clone this repo on your server:

```bash
git clone <your-repo-url> tunneld
cd tunneld
```

Create your `.env` file:

```bash
cp .env.example .env
nano .env
```

Set your values:

```env
TUNNEL_DOMAIN=tunnel.example.com
TUNNEL_PASSWORD=your-strong-password-here
TUNNEL_SSH_PORT=3017
TUNNEL_HTTP_PORT=3018
```

Start the server:

```bash
docker compose up -d
```

Check logs:

```bash
docker compose logs -f
```

You should see:

```
tunneld  | Generating SSH host key at /data/host_key
tunneld  | Password auth enabled
tunneld  | SSH server listening on :3017
tunneld  | HTTP proxy listening on :3018
tunneld  | Tunnels will be available at https://<id>.tunnel.example.com
```

### Step 4: Firewall

Make sure port **3017** (or your configured `TUNNEL_SSH_PORT`) is open on your server's firewall. Port 3018 only needs to be accessible from localhost (Caddy).

```bash
# Example: ufw
sudo ufw allow 3017/tcp
```

## Usage

### Basic tunnel

Expose a local web server running on port 3000:

```bash
ssh -p 3017 tunnel@tunnel.example.com -R 0:localhost:3000
```

You get a random subdomain like `https://a3f2b1c8.tunnel.example.com`.

### Custom subdomain

Use the SSH **username** to pick your subdomain:

```bash
ssh -p 3017 myapp@tunnel.example.com -R 0:localhost:3000
# → https://myapp.tunnel.example.com
```

Rules:
- Lowercase letters, numbers, and hyphens only
- Must be at least 2 characters
- If the name is already taken by another active tunnel, you get a random one instead

### Multiple ports

Expose multiple services at once:

```bash
ssh -p 3017 t@tunnel.example.com \
  -R 0:localhost:3000 \
  -R 0:localhost:8080
```

Each `-R` gets its own subdomain.

### Background tunnel

Run the tunnel in the background:

```bash
ssh -p 3017 -f -N t@tunnel.example.com -R 0:localhost:3000
```

- `-f` — go to background after authenticating
- `-N` — no shell (just the tunnel)

Note: with `-N`, the tunnel URL won't be printed to your terminal (no session channel). Check the server logs instead:

```bash
docker compose logs --tail=5
```

### Shell alias

Add to your `~/.bashrc` or `~/.zshrc` for convenience:

```bash
tunnel() {
  local port="${1:-3000}"
  local name="${2:-t}"
  ssh -p 3017 "${name}@tunnel.example.com" -R "0:localhost:${port}"
}
```

Then:

```bash
tunnel 3000              # random subdomain
tunnel 3000 myapp        # https://myapp.tunnel.example.com
tunnel 8080 api          # https://api.tunnel.example.com
```

### Using SSH keys instead of (or with) password

If you prefer key-based auth, copy your public key to the server:

```bash
# On your local machine
cat ~/.ssh/id_ed25519.pub
# Copy the output
```

On the server, create an `authorized_keys` file and mount it:

```bash
# Create the file with your public key
echo "ssh-ed25519 AAAA... you@machine" > authorized_keys
```

Update `docker-compose.yml` to mount it:

```yaml
services:
  tunneld:
    # ... existing config ...
    volumes:
      - tunnel-data:/data
      - ./authorized_keys:/authorized_keys:ro
    environment:
      - TUNNEL_DOMAIN=${TUNNEL_DOMAIN}
      - TUNNEL_PASSWORD=${TUNNEL_PASSWORD}           # keep for password auth
      - TUNNEL_AUTHORIZED_KEYS=/authorized_keys      # add for key auth
```

Now both password and key auth work. Remove `TUNNEL_PASSWORD` if you want key-only auth.

## Configuration

All options can be set via flags or environment variables:

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `-domain` | `TUNNEL_DOMAIN` | *(required)* | Base domain for tunnel URLs |
| `-password` | `TUNNEL_PASSWORD` | | Password for SSH authentication |
| `-authorized-keys` | `TUNNEL_AUTHORIZED_KEYS` | | Path to authorized_keys file |
| `-ssh-addr` | `TUNNEL_SSH_ADDR` | `:3017` | SSH server listen address |
| | `TUNNEL_SSH_PORT` | `3017` | SSH port (used by docker-compose for port mapping) |
| | `TUNNEL_HTTP_PORT` | `3018` | HTTP port (used by docker-compose for port mapping) |
| `-http-addr` | `TUNNEL_HTTP_ADDR` | `:3018` | HTTP proxy listen address |
| `-host-key` | `TUNNEL_HOST_KEY` | `host_key` | Path to SSH host key (auto-generated if missing) |

At least one of `-password` or `-authorized-keys` must be provided.

## Running without Docker

If you prefer to run the binary directly:

```bash
# Install Go 1.22+
# https://go.dev/dl/

# Build
go build -o tunneld .

# Run
./tunneld -domain tunnel.example.com -password 'your-password'
```

Cross-compile for Linux from another OS:

```bash
GOOS=linux GOARCH=amd64 go build -o tunneld .
```

## Architecture

```
tunnel/
├── main.go      # Entry point, config parsing, host key generation
├── ssh.go       # SSH server: auth, session handling, tcpip-forward
├── proxy.go     # HTTP reverse proxy: routes requests by subdomain
├── tunnel.go    # Tunnel struct, registry (subdomain → SSH connection)
├── Dockerfile   # Multi-stage build: Go builder → Alpine runtime
└── docker-compose.yml
```

**SSH flow:** Client connects → authenticates (password or key) → sends `tcpip-forward` request → server generates subdomain, registers tunnel, replies → server prints URL to client's session.

**HTTP flow:** Request arrives → extract subdomain from Host header → look up tunnel in registry → open `forwarded-tcpip` SSH channel → forward HTTP request → stream response back.

**WebSocket flow:** Same as HTTP, but after detecting the `Upgrade: websocket` header, the proxy hijacks the connection and does raw bidirectional byte copying through the SSH channel.

## Troubleshooting

### "Tunnel not found" when visiting the URL

- Check the tunnel is still active (SSH connection alive)
- Verify DNS: `dig abc123.tunnel.example.com` should resolve to your server IP
- Check Caddy is proxying to port 3018: `curl -H "Host: abc123.tunnel.example.com" http://localhost:3018`

### SSH connection refused

- Check port is open: `nc -zv tunnel.example.com 3017`
- Check the container is running: `docker compose ps`
- Check logs: `docker compose logs tunneld`

### Host key verification failed

The host key is auto-generated on first start. If you recreate the container volume, the key changes and SSH clients will complain. Fix:

```bash
ssh-keygen -R "[tunnel.example.com]:3017"
```

### Caddy not issuing wildcard cert

Wildcard certs require DNS challenge. Make sure you have the correct [Caddy DNS plugin](https://caddyserver.com/docs/modules/) installed and configured with your DNS provider's API token.
