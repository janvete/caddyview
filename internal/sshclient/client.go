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

// TailLines reads the last n lines of a remote file.
func (c *Client) TailLines(path string, n int) ([]string, error) {
	out, err := c.RunCommand(fmt.Sprintf("tail -n %d %s 2>/dev/null", n, path))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// StreamLines streams new lines from a remote file (like tail -f) into the channel.
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

	if err := session.Start(fmt.Sprintf("tail -n 0 -f %s 2>/dev/null", path)); err != nil {
		session.Close()
		return err
	}

	go func() {
		defer session.Close()
		scanner := bufio.NewScanner(stdout)
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

// LoadAggregatedHistory aggregates log data on the remote server using awk,
// returning only summary buckets instead of raw lines. This is critical for
// high-traffic servers where raw line transfer would be impractical.
//
// bucketSecs: size of each time bucket in seconds (e.g. 3600 for hourly)
// maxAgeDays: find only files modified within this many days (performance filter)
func (c *Client) LoadAggregatedHistory(logPath string, since, bucketSecs int64, maxAgeDays int) (*parser.AggregatedHistory, error) {
	lastSlash := strings.LastIndex(logPath, "/")
	dir, base := "/", logPath
	if lastSlash >= 0 {
		dir = logPath[:lastSlash]
		if dir == "" {
			dir = "/"
		}
		base = logPath[lastSlash+1:]
	}

	// awk aggregation script — runs entirely on the server.
	// Parses JSON log lines, groups by time bucket, outputs summary.
	// Uses only POSIX-compatible awk constructs.
	awkProg := `
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
  tot++; totb+=sz
}
END {
  print "BUCKETS"
  for (b in bc) print b, bc[b], bb[b], bs2[b]+0, bs3[b]+0, bs4[b]+0, bs5[b]+0
  print "HOSTS"
  for (h in hc) print hc[h], h
  print "IPS"
  for (i in ic) print ic[i], i
  print "TOTALS"
  print tot+0, totb+0
}`

	// Cache file is keyed by log path + bucket size (not since, which changes every second).
	// Cache is valid as long as no new .gz file appeared since it was written.
	// A log rotation (every ~9 min on high-traffic servers) is the only invalidation event.
	cacheKey := strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(base)
	cachePath := fmt.Sprintf("/tmp/caddyview_%s_%d.cache", cacheKey, bucketSecs)

	findPipe := fmt.Sprintf(
		`find %s -name '%s*' -type f -mtime -%d 2>/dev/null | sort -r | `+
			`while read f; do case "$f" in *.gz) zcat "$f" ;; *) cat "$f" ;; esac; done 2>/dev/null | `+
			`awk -v cutoff=%d -v bucket=%d '%s'`,
		dir, base, maxAgeDays+1, since, bucketSecs, awkProg,
	)

	// Wrap with server-side cache: hit → instant cat; miss → run awk and tee to cache atomically.
	cmd := fmt.Sprintf(
		`CACHE=%s; `+
			`LATEST=$(find %s -name '%s*.gz' -mtime -%d 2>/dev/null | sort | tail -1); `+
			`if [ -f "$CACHE" ] && { [ -z "$LATEST" ] || [ "$CACHE" -nt "$LATEST" ]; }; then `+
			`cat "$CACHE"; `+
			`else `+
			`TMPFILE=$(mktemp /tmp/caddyview.XXXXXX 2>/dev/null || echo /tmp/caddyview_fallback.tmp); `+
			`%s | tee "$TMPFILE" && mv "$TMPFILE" "$CACHE" 2>/dev/null || cat "$TMPFILE" 2>/dev/null; `+
			`fi`,
		cachePath, dir, base, maxAgeDays+1, findPipe,
	)

	out, err := c.RunCommand(cmd)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("aggregation failed: %w", err)
	}
	return parser.ParseAggregatedOutput(out)
}
