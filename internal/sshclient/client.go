package sshclient

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
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
	candidates := []string{}
	if keyPath != "" {
		candidates = append(candidates, keyPath)
	} else {
		home, _ := os.UserHomeDir()
		candidates = append(candidates,
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
		)
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
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	return nil, fmt.Errorf("no usable SSH private key found (tried: %v)", candidates)
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

// ensure net import is used (used in future dial variants)
var _ = net.IPv4len
