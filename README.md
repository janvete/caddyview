# Caddyview

Remote Caddy log viewer over SSH.

## Installation

### macOS (Homebrew)

```bash
brew tap janvete/caddyview
brew install caddyview
```

### Linux

```bash
curl -L https://github.com/janvete/caddyview/releases/latest/download/caddyview-v0.6.0-linux-amd64.tar.gz | tar xz
sudo mv caddyview /usr/local/bin/
```

## Usage

```bash
caddyview ssh root@192.168.x.x
caddyview ssh -p 2222 admin@server.com
```

## Required Caddyfile Changes

### 1. Add a log block to your Caddyfile

Add the logging configuration to a [snippet](https://caddyserver.com/docs/caddyfile/concepts#snippets) or to each individual site block:

```caddyfile
(hsts) {
    log {
        output file /var/log/caddy/access.log {
            roll_size 100mb
            roll_keep 10
        }
        format json
    }
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains; preload"
        X-Content-Type-Options "nosniff"
        Referrer-Policy "strict-origin-when-cross-origin"
        Permissions-Policy "geolocation=(), microphone=(), camera=()"
        -Server
    }
}

some.dns.com {
    import hsts
    reverse_proxy 192.168.71.6:1378
}
```

### 2. Create the log file with correct permissions (BEFORE reloading!)

```bash
mkdir -p /var/log/caddy
touch /var/log/caddy/access.log
chown caddy:caddy /var/log/caddy/access.log
```

### 3. Reload Caddy

```bash
systemctl reload caddy
```
