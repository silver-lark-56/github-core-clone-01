// http.go: a hand-rolled HTTP/1.1 server over raw TCP (package net).
// No net/http: the response path is one Write of pre-built bytes, and
// the binary stays small. Keep-alive is honoured; every response
// carries Content-Length and X-GitHub-Request-Id.
package main

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"strings"
	"time"
)

// request is one parsed HTTP request.
type request struct {
	method  string
	path    string // decoded path, no query
	rawPath string
	query   string
	body    []byte
	host    string
	// The only two headers any handler reads; keeping them as fields
	// avoids a map allocation and a lower-case per header line.
	auth        string // Authorization
	gitProtocol string // Git-Protocol
	// keepAlive reports whether the connection stays open after this
	// request (HTTP/1.1 default, unless Connection: close).
	keepAlive bool
}

func (q *request) header(name string) string {
	switch name {
	case "authorization":
		return q.auth
	case "git-protocol":
		return q.gitProtocol
	}
	return ""
}

// queryValue returns the first decoded value of a query parameter.
func (q *request) queryValue(key string) (string, bool) {
	for _, pair := range strings.Split(q.query, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if decodeQuery(k) == key {
			return decodeQuery(v), true
		}
	}
	return "", false
}

// queryInt returns a query parameter as an int, or def.
func (q *request) queryInt(key string, def int) int {
	v, ok := q.queryValue(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// decodeQuery decodes %XX and + (space) in a query component.
func decodeQuery(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s):
			if hi, ok1 := hexVal(s[i+1]); ok1 {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// decodePath decodes %XX in a path segment (+ stays literal).
func decodePath(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			if hi, ok1 := hexVal(s[i+1]); ok1 {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// response is one HTTP response, written in a single Write call.
type response struct {
	status  int
	headers [][2]string
	body    []byte
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	}
	return "Status"
}

// server is the forge HTTP server.
type server struct {
	store *Store
	token string // MAKEFUN_SHOP_TOKEN; "" accepts any non-empty bearer
}

func newServer(store *Store, token string) *server {
	return &server{store: store, token: token}
}

// serve accepts connections forever on ln.
func (s *server) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()
	// The trial pod kills us at 900 s; a generous idle timeout keeps
	// abandoned keep-alive connections from piling up goroutines.
	br := bufio.NewReaderSize(conn, 65536)
	for {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		req, err := readRequest(br)
		if err != nil {
			return
		}
		resp := s.dispatch(req)
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := writeResponse(conn, req, resp); err != nil {
			return
		}
		if !req.keepAlive {
			return
		}
	}
}

// readRequest parses one request from br (headers + body).
func readRequest(br *bufio.Reader) (*request, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, errBadRequest
	}
	req := &request{
		method:    parts[0],
		keepAlive: true, // HTTP/1.1 default
	}
	rawTarget := parts[1]
	if i := strings.IndexByte(rawTarget, '?'); i >= 0 {
		req.rawPath = rawTarget[:i]
		req.query = rawTarget[i+1:]
	} else {
		req.rawPath = rawTarget
	}
	req.path = decodePathSegments(req.rawPath)
	if strings.HasPrefix(parts[2], "HTTP/1.0") {
		req.keepAlive = false
	}
	var contentLength int = -1
	chunked := false
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		switch key {
		case "content-length":
			contentLength, _ = strconv.Atoi(val)
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(val), "chunked") {
				chunked = true
			}
		case "connection":
			lv := strings.ToLower(val)
			if strings.Contains(lv, "close") {
				req.keepAlive = false
			} else if strings.Contains(lv, "keep-alive") {
				req.keepAlive = true
			}
		case "host":
			req.host = val
		case "authorization":
			req.auth = val
		case "git-protocol":
			req.gitProtocol = val
		}
	}
	if chunked {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		req.body = body
	} else if contentLength > 0 {
		if contentLength > 64<<20 {
			return nil, errBadRequest
		}
		body := make([]byte, contentLength)
		if _, err := readFull(br, body); err != nil {
			return nil, err
		}
		req.body = body
	}
	return req, nil
}

// decodePathSegments decodes each %-encoded segment of a path.
func decodePathSegments(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = decodePath(seg)
	}
	return strings.Join(segs, "/")
}

func readFull(br *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := br.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(line, 16, 64)
		if err != nil || n < 0 || n > 64<<20 {
			return nil, errBadRequest
		}
		if n == 0 {
			// Trailer headers until a blank line.
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
					return out, nil
				}
			}
		}
		chunk := make([]byte, n)
		if _, err := readFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// Trailing CRLF after the chunk.
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
		if len(out) > 64<<20 {
			return nil, errBadRequest
		}
	}
}

type httpError string

func (e httpError) Error() string { return string(e) }

const errBadRequest = httpError("bad request")

// writeResponse serializes one response in a single Write.
func writeResponse(conn net.Conn, req *request, resp *response) error {
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(resp.status))
	b.WriteByte(' ')
	b.WriteString(statusText(resp.status))
	b.WriteString("\r\n")
	for _, h := range resp.headers {
		b.WriteString(h[0])
		b.WriteString(": ")
		b.WriteString(h[1])
		b.WriteString("\r\n")
	}
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(len(resp.body)))
	b.WriteString("\r\n")
	if !req.keepAlive {
		b.WriteString("Connection: close\r\n")
	}
	b.WriteString("\r\n")
	// One writev of headers + body: no copy of the body (a clone
	// response can be hundreds of KiB).
	nb := net.Buffers{b.Bytes(), resp.body}
	_, err := nb.WriteTo(conn)
	return err
}

// --- response constructors (every reply carries X-GitHub-Request-Id) ---

func (s *server) jsonResp(status int, body []byte, extra ...[2]string) *response {
	h := [][2]string{
		{"Content-Type", "application/json"},
		{"X-GitHub-Request-Id", s.store.nextRequestID()},
	}
	h = append(h, extra...)
	return &response{status: status, headers: h, body: body}
}

func (s *server) jsonErr(status int, msg string) *response {
	w := newJB(64)
	w.obj().strField("message", msg).end()
	return s.jsonResp(status, w.bytes())
}

func (s *server) emptyResp(status int) *response {
	return &response{
		status: status,
		headers: [][2]string{
			{"Content-Type", "application/json"},
			{"X-GitHub-Request-Id", s.store.nextRequestID()},
		},
	}
}

// authed reports whether the request carries the expected bearer token.
// A missing or wrong token is 401; there is no other auth in this forge.
func (s *server) authed(req *request) bool {
	h := req.header("authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := strings.TrimPrefix(h, "Bearer ")
	if tok == "" {
		return false
	}
	if s.token == "" {
		return true
	}
	return tok == s.token
}

// requireAuth answers the 401 for an unauthenticated request, or nil
// when the request may proceed.
func (s *server) requireAuth(req *request) *response {
	if !s.authed(req) {
		return s.jsonErr(401, "Requires authentication")
	}
	return nil
}

// parseJSONBody parses a request body as a JSON object. A malformed body
// is a 400; an empty body is an empty object.
func (s *server) parseJSONBody(req *request) (map[string]any, *response) {
	if len(req.body) == 0 {
		return map[string]any{}, nil
	}
	v, err := ParseJSON(req.body)
	if err != nil {
		return nil, s.jsonErr(400, "Invalid JSON body")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, s.jsonErr(400, "JSON body must be an object")
	}
	return m, nil
}

// dispatch routes one request.
func (s *server) dispatch(req *request) (resp *response) {
	defer func() {
		// A handler must never kill the connection loop: a panic is a
		// 500 JSON reply, and the server keeps serving.
		if r := recover(); r != nil {
			resp = s.jsonErr(500, "Internal server error")
		}
	}()
	p := req.path

	// Health probe: no auth, answered twice at boot.
	if p == "/healthz" && req.method == "GET" {
		return s.jsonResp(200, []byte(`{"ok":true}`))
	}

	// MCP endpoint.
	if p == "/mcp" {
		return s.handleMCP(req)
	}

	// GraphQL endpoint.
	if p == "/api/graphql" {
		if req.method != "POST" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.handleGraphQL(req)
	}

	// REST v3.
	if p == "/api/v3" || strings.HasPrefix(p, "/api/v3/") {
		return s.handleREST(req)
	}

	// Git smart HTTP: /{owner}/{repo}.git/<service>.
	if strings.HasSuffix(p, ".git/info/refs") || strings.HasSuffix(p, ".git/git-upload-pack") || strings.HasSuffix(p, ".git/git-receive-pack") {
		return s.handleGit(req)
	}

	return s.jsonErr(404, "Not Found")
}
