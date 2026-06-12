package sshclient

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/janvete/caddyview/internal/parser"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Client struct {
	conn *ssh.Client
	host string
}

func Connect(target string, port int, keyPath string) (*Client, error) {
	user, host, err := parseTarget(target)
	if err != nil {
		return nil, err
	}

	authMethods, err := buildAuthMethods(keyPath)
	if err != nil {
		return nil, fmt.Errorf("auth setup failed: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	return &Client{conn: conn, host: host}, nil
}

func (c *Client) Close() {
	c.conn.Close()
}

func (c *Client) Host() string {
	return c.host
}

// RunCommand runs a command and returns its output as a string.
func (c *Client) RunCommand(cmd string) (string, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	out, err := session.Output(cmd)
	return string(out), err
}

// StreamLines streams new lines from a remote file (like tail -F) into the channel.
func (c *Client) StreamLines(path string, lines chan<- string, done <-chan struct{}) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return err
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return err
	}

	// -F follows by name: Caddy rotates by renaming the file, which would
	// silently kill a plain -f stream at every rotation.
	if err := session.Start(fmt.Sprintf("tail -n 0 -F %s 2>/dev/null", path)); err != nil {
		session.Close()
		return err
	}

	go func() {
		defer session.Close()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case <-done:
				return
			case lines <- scanner.Text():
			}
		}
	}()

	go func() {
		<-done
		session.Close()
	}()

	return nil
}

// SysStats fetches CPU and memory usage from the remote server.
func (c *Client) SysStats() (cpuPercent float64, memUsedMB, memTotalMB int64, err error) {
	// Memory from /proc/meminfo
	memOut, err := c.RunCommand("cat /proc/meminfo")
	if err != nil {
		return 0, 0, 0, err
	}
	memTotalMB, memUsedMB = parseMeminfo(memOut)

	// CPU: two /proc/stat samples 500ms apart
	cpuOut, err := c.RunCommand("cat /proc/stat; sleep 0.5; cat /proc/stat")
	if err != nil {
		return 0, memUsedMB, memTotalMB, err
	}
	cpuPercent = parseCPUStat(cpuOut)

	return cpuPercent, memUsedMB, memTotalMB, nil
}

func parseTarget(target string) (user, host string, err error) {
	parts := strings.SplitN(target, "@", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid target %q — expected user@host", target)
	}
	return parts[0], parts[1], nil
}

func buildAuthMethods(keyPath string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// SSH agent — uses whatever keys are loaded in the agent (macOS Keychain, ssh-add, etc.)
	if m := agentAuth(); m != nil {
		methods = append(methods, m)
	}

	// Explicit key file or common defaults
	var candidates []string
	if keyPath != "" {
		candidates = append(candidates, keyPath)
	} else {
		home, _ := os.UserHomeDir()
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			candidates = append(candidates, filepath.Join(home, ".ssh", name))
		}
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH auth method available — no agent (SSH_AUTH_SOCK) and no key files found in %v", candidates)
	}
	return methods, nil
}

// agentAuth returns an auth method backed by the SSH agent if SSH_AUTH_SOCK is set.
func agentAuth() ssh.AuthMethod {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	return ssh.PublicKeysCallback(agent.NewClient(conn).Signers)
}

func parseMeminfo(raw string) (totalMB, usedMB int64) {
	var total, available int64
	for _, line := range strings.Split(raw, "\n") {
		var val int64
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &val); err == nil {
			total = val
		}
		if _, err := fmt.Sscanf(line, "MemAvailable: %d kB", &val); err == nil {
			available = val
		}
	}
	return total / 1024, (total - available) / 1024
}

func parseCPUStat(raw string) float64 {
	type cpuSample struct{ user, nice, sys, idle, iowait, irq, softirq uint64 }
	parse := func(line string) (cpuSample, bool) {
		var s cpuSample
		var label string
		n, _ := fmt.Sscanf(line, "%s %d %d %d %d %d %d %d",
			&label, &s.user, &s.nice, &s.sys, &s.idle, &s.iowait, &s.irq, &s.softirq)
		return s, n >= 5 && label == "cpu"
	}

	var samples []cpuSample
	for _, line := range strings.Split(raw, "\n") {
		if s, ok := parse(line); ok {
			samples = append(samples, s)
			if len(samples) == 2 {
				break
			}
		}
	}
	if len(samples) < 2 {
		return 0
	}

	total1 := samples[0].user + samples[0].nice + samples[0].sys + samples[0].idle + samples[0].iowait + samples[0].irq + samples[0].softirq
	total2 := samples[1].user + samples[1].nice + samples[1].sys + samples[1].idle + samples[1].iowait + samples[1].irq + samples[1].softirq
	idle1 := samples[0].idle + samples[0].iowait
	idle2 := samples[1].idle + samples[1].iowait

	totalDiff := float64(total2 - total1)
	idleDiff := float64(idle2 - idle1)
	if totalDiff == 0 {
		return 0
	}
	return (totalDiff - idleDiff) / totalDiff * 100
}

// historyAwkProg aggregates Caddy JSON log lines into hourly summary buckets.
// It runs entirely on the server and uses only POSIX awk constructs (works
// with gawk, mawk and busybox awk). IPs are capped to the top 50 per file so
// the output stays small even on servers with millions of unique clients.
const historyAwkProg = `
BEGIN { OFS=" " }
{
  if (index($0,"\"msg\":\"handled request\"") == 0) next
  p = index($0,"\"ts\":")
  if (p == 0) next
  ts = substr($0,p+5,20)+0
  if (ts < cutoff) next
  b = int(ts/bucket)*bucket
  p = index($0,"\"status\":")
  st = (p>0) ? substr($0,p+9,5)+0 : 0
  p = index($0,"\"size\":")
  sz = (p>0) ? substr($0,p+7,15)+0 : 0
  p = index($0,"\"duration\":")
  du = (p>0) ? substr($0,p+11,14)+0 : 0
  p = index($0,"\"host\":\"")
  if (p>0) { rest=substr($0,p+8); q=index(rest,"\""); ho=(q>0)?substr(rest,1,q-1):"?" } else ho="?"
  p = index($0,"\"remote_ip\":\"")
  if (p>0) { rest=substr($0,p+13); q=index(rest,"\""); ip=(q>0)?substr(rest,1,q-1):"?" } else ip="?"
  bc[b]++; bb[b]+=sz
  if (st>=500) bs5[b]++
  else if (st>=400) bs4[b]++
  else if (st>=300) bs3[b]++
  else if (st>=200) bs2[b]++
  hc[ho]++; ic[ip]++
  tot++; totb+=sz; totd+=du
}
END {
  print "BUCKETS"
  for (b in bc) print b, bc[b], bb[b], bs2[b]+0, bs3[b]+0, bs4[b]+0, bs5[b]+0
  print "HOSTS"
  for (h in hc) print hc[h], h
  print "IPS"
  k = 50
  for (i in ic) n++
  while (k > 0 && n > 0) {
    max = -1; mi = ""
    for (i in ic) if (ic[i] > max) { max = ic[i]; mi = i }
    print ic[mi], mi
    delete ic[mi]
    k--; n--
  }
  print "TOTALS"
  print tot+0, totb+0, totd+0
}`

// LoadAggregatedHistory aggregates log data on the remote server using awk,
// returning only summary buckets instead of raw lines. This is critical for
// high-traffic servers where raw line transfer would be impractical.
//
// Rotated log files are immutable, so each one is aggregated exactly once and
// the result is cached on the server (/tmp/caddyview_*.cache); after a
// rotation only the single new file is scanned. The current log file is always
// scanned fresh, so the view never goes stale. Buckets are always hourly; the
// caller re-buckets client-side, which lets all tabs share the same caches.
//
// since: unix cutoff for the current (still growing) log file
// maxAgeDays: only look at files modified within this many days
func (c *Client) LoadAggregatedHistory(logPath string, since int64, maxAgeDays int) (*parser.AggregatedHistory, error) {
	lastSlash := strings.LastIndex(logPath, "/")
	dir, base := "/", logPath
	if lastSlash >= 0 {
		dir = logPath[:lastSlash]
		if dir == "" {
			dir = "/"
		}
		base = logPath[lastSlash+1:]
	}

	// Match both Caddy's native rotation naming (access-2026-06-12T10-30-00.000.log
	// and .log.gz next to access.log) and classic logrotate naming (access.log.1.gz).
	stem, ext := base, ""
	if dot := strings.LastIndex(base, "."); dot > 0 {
		stem, ext = base[:dot], base[dot:]
	}
	namePattern := fmt.Sprintf(
		`\( -name '%s' -o -name '%s.*' -o -name '%s-*%s' -o -name '%s-*%s.gz' \)`,
		base, base, stem, ext, stem, ext,
	)

	cmd := fmt.Sprintf(
		`find /tmp -maxdepth 1 -name 'caddyview_*' -mtime +8 -delete 2>/dev/null; `+
			`find %s %s -type f -mtime -%d 2>/dev/null | sort | while read -r f; do `+
			`if [ "$f" = "%s" ]; then `+
			`awk -v cutoff=%d -v bucket=3600 '%s' < "$f"; `+
			`else `+
			`key=$(printf '%%s' "$f" | tr -c 'A-Za-z0-9' '_'); c="/tmp/caddyview_${key}.cache"; `+
			`if [ -s "$c" ] && [ "$c" -nt "$f" ]; then cat "$c"; else `+
			`t=$(mktemp /tmp/caddyview.XXXXXX 2>/dev/null) || t="/tmp/caddyview_tmp.$$"; `+
			`case "$f" in *.gz) zcat "$f" ;; *) cat "$f" ;; esac 2>/dev/null `+
			`| awk -v cutoff=0 -v bucket=3600 '%s' > "$t" && mv "$t" "$c" && cat "$c" || cat "$t" 2>/dev/null; `+
			`fi; fi; done`,
		dir, namePattern, maxAgeDays+1,
		logPath,
		since, historyAwkProg,
		historyAwkProg,
	)

	out, err := c.RunCommand(cmd)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("aggregation failed: %w", err)
	}
	return parser.ParseAggregatedOutput(out)
}
