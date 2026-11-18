// forge: a standalone GitHub-compatible git forge in one process —
// git smart HTTP, REST v3, GraphQL and MCP over one in-memory store.
//
// Lifecycle (the entry <-> env contract): bind an ephemeral port,
// POST {"port": N} to $MAKEFUN_ENV_URL/register, serve until GET
// $MAKEFUN_ENV_URL/done answers {"done": true}, then exit 0. Without
// MAKEFUN_ENV_URL the forge serves locally on $FORGE_PORT (default
// 8080) for the SDK's local runner.
package main

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultNowMs is the fixed "now" when MAKEFUN_TRIAL_NOW is unset
// (local runs): the final phase's opening instant, 2026-11-21T06:40:00Z.
const defaultNowMs = 1795243200000

func main() {
	os.Exit(run())
}

func run() int {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	nowMs := parseTrialNow(os.Getenv("MAKEFUN_TRIAL_NOW"))
	store := NewStore(nowMs)
	srv := newServer(store, token)

	if envURL == "" {
		// Local mode: serve forever on a fixed port for sdk/run.ts.
		port := os.Getenv("FORGE_PORT")
		if port == "" {
			port = "8080"
		}
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			stderr("forge: listen: " + err.Error() + "\n")
			return 1
		}
		stdout("forge: listening on port " + port + " (local mode, no MAKEFUN_ENV_URL)\n")
		srv.serve(ln)
		return 0
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		stderr("forge: listen: " + err.Error() + "\n")
		return 1
	}
	addr := ln.Addr().(*net.TCPAddr)
	stdout("forge: listening on port " + strconv.Itoa(addr.Port) + "\n")
	go srv.serve(ln)

	if err := register(envURL, addr.Port); err != nil {
		stderr("forge: register: " + err.Error() + "\n")
		return 1
	}
	stdout("forge: registered; waiting for the env to say done\n")
	waitForDone(envURL)
	stdout("forge: done\n")
	return 0
}

// parseTrialNow parses MAKEFUN_TRIAL_NOW: an RFC 3339 instant or epoch
// milliseconds. Anything else (or empty) is the fixed default.
func parseTrialNow(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultNowMs
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return ms
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	// RFC 3339 with fractional seconds and no timezone is not expected;
	// try the common layouts before falling back.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return defaultNowMs
}

func stdout(s string) { os.Stdout.WriteString(s) }
func stderr(s string) { os.Stderr.WriteString(s) }

// --- the env client: register + done, over raw TCP ---

// httpRoundTrip performs one minimal HTTP/1.1 request and returns the
// status code and body. Only http:// URLs are supported (the env is
// plain HTTP); the connection is closed after each request.
func httpRoundTrip(url, method, body string) (int, []byte, error) {
	rest := strings.TrimPrefix(url, "http://")
	host, path, _ := strings.Cut(rest, "/")
	path = "/" + path
	if i := strings.IndexByte(host, '?'); i >= 0 {
		host = host[:i]
	}
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 30*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req strings.Builder
	req.WriteString(method + " " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + host + "\r\n")
	if body != "" {
		req.WriteString("Content-Type: application/json\r\n")
		req.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	req.WriteString("Connection: close\r\n\r\n")
	req.WriteString(body)
	if _, err := conn.Write([]byte(req.String())); err != nil {
		return 0, nil, err
	}
	// Read until EOF (Connection: close).
	var raw []byte
	buf := make([]byte, 8192)
	for {
		n, err := conn.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	head, respBody, ok := cutBytes(raw, []byte("\r\n\r\n"))
	if !ok {
		return 0, nil, errBadResponse
	}
	// Status line: HTTP/1.1 <code> <text>
	line := head
	if i := indexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	parts := strings.SplitN(string(line), " ", 3)
	if len(parts) < 2 {
		return 0, nil, errBadResponse
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, errBadResponse
	}
	return code, respBody, nil
}

type envError string

func (e envError) Error() string { return string(e) }

const errBadResponse = envError("bad response from env")

func cutBytes(b, sep []byte) (before, after []byte, ok bool) {
	for i := 0; i+len(sep) <= len(b); i++ {
		match := true
		for j := range sep {
			if b[i+j] != sep[j] {
				match = false
				break
			}
		}
		if match {
			return b[:i], b[i+len(sep):], true
		}
	}
	return nil, nil, false
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// register POSTs the bound port until the env accepts it (200, or 409
// when a first register already won). 400/405 are contract errors and
// end the program; anything else is retried with backoff.
func register(envURL string, port int) error {
	body := `{"port":` + strconv.Itoa(port) + `}`
	delay := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		code, _, err := httpRoundTrip(envURL+"/register", "POST", body)
		if err == nil {
			if code == 200 || code == 409 {
				return nil
			}
			if code == 400 || code == 405 {
				return envError("register answered HTTP " + strconv.Itoa(code))
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
		if attempt > 200 {
			return envError("register: the env never answered")
		}
	}
}

// waitForDone polls GET /done until {"done": true}. It never returns
// before that (a failed poll is retried with backoff).
func waitForDone(envURL string) {
	delay := 250 * time.Millisecond
	for {
		code, body, err := httpRoundTrip(envURL+"/done", "GET", "")
		if err == nil && code == 200 {
			if v, err := ParseJSON(body); err == nil {
				if m, ok := v.(map[string]any); ok {
					if done, _ := m["done"].(bool); done {
						return
					}
				}
			}
			delay = 250 * time.Millisecond
		} else {
			time.Sleep(delay)
			if delay < 2*time.Second {
				delay *= 2
			}
			continue
		}
		time.Sleep(delay)
	}
}
