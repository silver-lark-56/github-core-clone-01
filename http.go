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
	"sync"
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
// headers is a fixed array (never more than Content-Type + 3 extras),
// and the request id travels as a number: formatting "forge-N" happens
// inline in writeResponse, so no per-response string or slice is
// allocated for the two headers every reply carries.
type response struct {
	status   int
	reqID    uint64
	headers  [4][2]string
	nheaders int
	body     []byte
}

func (r *response) addHeader(k, v string) {
	if r.nheaders < len(r.headers) {
		r.headers[r.nheaders] = [2]string{k, v}
		r.nheaders++
	}
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
	// 8 KiB is plenty for request lines + headers (bodies are read
	// into their own slices); 64 KiB per connection was pure GC load
	// on the 50-concurrent-clone workload.
	br := bufio.NewReaderSize(conn, 8192)
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

// readLine reads one \n-terminated line from br without allocating
// when it fits the buffer: the slice borrows br's buffer and is only
// valid until the next read, so callers must copy anything they keep.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// Over-long line: accumulate (rare; headers are short).
		buf := append([]byte(nil), line...)
		for err == bufio.ErrBufferFull {
			line, err = br.ReadSlice('\n')
			buf = append(buf, line...)
		}
		return buf, err
	}
	return line, err
}

// trimLine drops trailing \r and \n bytes (in place, no allocation).
func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	return b
}

// trimSpaces drops leading/trailing spaces and tabs (in place).
func trimSpaces(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// headerKeyIs reports whether k (a raw header name) equals want
// (lower-case) without allocating.
func headerKeyIs(k []byte, want string) bool {
	if len(k) != len(want) {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// containsFoldFold reports whether v contains needle, case-insensitively.
func containsFold(v []byte, needle string) bool {
	if len(needle) == 0 || len(v) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(v); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			c := v[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// readRequest parses one request from br (headers + body).
func readRequest(br *bufio.Reader) (*request, error) {
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	line = trimLine(line)
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := -1
	if sp1 >= 0 {
		sp2 = bytes.IndexByte(line[sp1+1:], ' ')
		if sp2 >= 0 {
			sp2 += sp1 + 1
		}
	}
	if sp1 < 0 || sp2 < 0 {
		return nil, errBadRequest
	}
	req := &request{
		method:    string(line[:sp1]),
		keepAlive: true, // HTTP/1.1 default
	}
	rawTarget := line[sp1+1 : sp2]
	if i := bytes.IndexByte(rawTarget, '?'); i >= 0 {
		req.rawPath = string(rawTarget[:i])
		req.query = string(rawTarget[i+1:])
	} else {
		req.rawPath = string(rawTarget)
	}
	req.path = decodePathSegments(req.rawPath)
	if bytes.HasPrefix(line[sp2+1:], []byte("HTTP/1.0")) {
		req.keepAlive = false
	}
	var contentLength int = -1
	chunked := false
	for {
		h, err := readLine(br)
		if err != nil {
			return nil, err
		}
		h = trimLine(h)
		if len(h) == 0 {
			break
		}
		colon := bytes.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		key := trimSpaces(h[:colon])
		val := trimSpaces(h[colon+1:])
		switch {
		case headerKeyIs(key, "content-length"):
			contentLength, _ = strconv.Atoi(string(val))
		case headerKeyIs(key, "transfer-encoding"):
			if containsFold(val, "chunked") {
				chunked = true
			}
		case headerKeyIs(key, "connection"):
			if containsFold(val, "close") {
				req.keepAlive = false
			} else if containsFold(val, "keep-alive") {
				req.keepAlive = true
			}
		case headerKeyIs(key, "host"):
			req.host = string(val)
		case headerKeyIs(key, "authorization"):
			req.auth = string(val)
		case headerKeyIs(key, "git-protocol"):
			req.gitProtocol = string(val)
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
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		line = trimSpaces(trimLine(line))
		if i := bytes.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(string(line), 16, 64)
		if err != nil || n < 0 || n > 64<<20 {
			return nil, errBadRequest
		}
		if n == 0 {
			// Trailer headers until a blank line.
			for {
				t, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if len(trimLine(t)) == 0 {
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
		if _, err := readLine(br); err != nil {
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

// headerPool reuses response-header buffers: a fresh bytes.Buffer
// grows through 2-3 allocations per response; a pooled 256-byte
// buffer covers every header we emit.
var headerPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)
		return &b
	},
}

// writeResponse serializes one response in a single Write.
func writeResponse(conn net.Conn, req *request, resp *response) error {
	bp := headerPool.Get().(*[]byte)
	b := (*bp)[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(resp.status), 10)
	b = append(b, ' ')
	b = append(b, statusText(resp.status)...)
	b = append(b, "\r\n"...)
	b = append(b, "X-GitHub-Request-Id: forge-"...)
	b = strconv.AppendUint(b, resp.reqID, 10)
	b = append(b, "\r\n"...)
	for _, h := range resp.headers[:resp.nheaders] {
		b = append(b, h[0]...)
		b = append(b, ": "...)
		b = append(b, h[1]...)
		b = append(b, "\r\n"...)
	}
	b = append(b, "Content-Length: "...)
	b = strconv.AppendInt(b, int64(len(resp.body)), 10)
	b = append(b, "\r\n"...)
	if !req.keepAlive {
		b = append(b, "Connection: close\r\n"...)
	}
	b = append(b, "\r\n"...)
	// One writev of headers + body: no copy of the body (a clone
	// response can be hundreds of KiB).
	nb := net.Buffers{b, resp.body}
	_, err := nb.WriteTo(conn)
	*bp = b[:0]
	headerPool.Put(bp)
	return err
}

// --- response constructors (every reply carries X-GitHub-Request-Id) ---

func (s *server) jsonResp(status int, body []byte, extra ...[2]string) *response {
	r := &response{status: status, reqID: s.store.nextRequestSeq(), body: body}
	r.addHeader("Content-Type", "application/json")
	for _, e := range extra {
		r.addHeader(e[0], e[1])
	}
	return r
}

func (s *server) jsonErr(status int, msg string) *response {
	w := newJB(64)
	w.obj().strField("message", msg).end()
	return s.jsonResp(status, w.bytes())
}

func (s *server) emptyResp(status int) *response {
	r := &response{status: status, reqID: s.store.nextRequestSeq()}
	r.addHeader("Content-Type", "application/json")
	return r
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
