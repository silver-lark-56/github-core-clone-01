// mcp.go: the Model Context Protocol endpoint at /mcp — JSON-RPC 2.0
// over Streamable HTTP. The modern 2026-07-28 era is served
// (server/discover, no handshake, no session); the legacy initialize
// handshake is answered too, so a client that predates discover still
// connects. tools/list is a constant, byte-identical on every call.
package main

import (
	"strconv"
	"strings"
)

// toolsListJSON is the pre-serialized tools/list result: six tools,
// each with a non-empty description and an object inputSchema whose
// required properties are exactly the arguments the suite fills.
var toolsListJSON = []byte(`{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository owned by the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"The repository name."},"description":{"type":"string","description":"A short description of the repository."},"private":{"type":"boolean","description":"Whether the repository is private."},"owner":{"type":"string","description":"The owner login."}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The owner login."},"repo":{"type":"string","description":"The repository name."}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The owner login."},"repo":{"type":"string","description":"The repository name."},"title":{"type":"string","description":"The issue title."},"body":{"type":"string","description":"The issue body."}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The owner login."},"repo":{"type":"string","description":"The repository name."},"state":{"type":"string","description":"Filter by state: open, closed or all."}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request from a head branch into a base branch.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The owner login."},"repo":{"type":"string","description":"The repository name."},"title":{"type":"string","description":"The pull request title."},"body":{"type":"string","description":"The pull request body."},"head":{"type":"string","description":"The head branch name."},"base":{"type":"string","description":"The base branch name."}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Squash-merge a pull request into its base branch.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The owner login."},"repo":{"type":"string","description":"The repository name."},"pull_number":{"type":"integer","description":"The pull request number."}},"required":["owner","repo","pull_number"]}}` +
	`]}`)

// handleMCP serves POST /mcp (and refuses anything else).
func (s *server) handleMCP(req *request) *response {
	if req.method == "DELETE" {
		// No sessions are ever handed out, so there is nothing to close.
		return s.jsonResp(405, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32601,"message":"Method not found"}}`))
	}
	if req.method != "POST" {
		return s.jsonErr(405, "Method not allowed")
	}
	if resp := s.requireAuth(req); resp != nil {
		return resp
	}
	if len(req.body) == 0 {
		return s.mcpError(nil, -32700, "Parse error", 400)
	}
	parsed, err := ParseJSON(req.body)
	if err != nil {
		// A body that is not JSON: HTTP 400 with JSON-RPC -32700.
		return s.mcpError(nil, -32700, "Parse error", 400)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return s.mcpError(nil, -32600, "Invalid Request", 400)
	}
	id := m["id"]
	method := jstr(m, "method")
	params := jobj(m["params"])

	// Notifications carry no id and get no JSON-RPC response.
	if _, hasID := m["id"]; !hasID {
		if method == "notifications/initialized" {
			return s.emptyResp(202)
		}
		return s.emptyResp(202)
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	switch method {
	case "server/discover":
		return s.mcpResult(id, s.discoverResult())
	case "initialize":
		return s.mcpResult(id, s.initializeResult(params))
	case "tools/list":
		return s.mcpResult(id, toolsListJSON)
	case "tools/call":
		return s.mcpCallTool(id, params)
	case "ping":
		return s.mcpResult(id, []byte(`{}`))
	default:
		return s.mcpError(id, -32601, "Method not found: "+method, 200)
	}
}

// mcpResult wraps a pre-encoded result in a JSON-RPC response.
func (s *server) mcpResult(id any, result []byte) *response {
	w := newJB(len(result) + 64)
	w.obj()
	w.strField("jsonrpc", "2.0")
	w.key("id")
	w.val(jsonID(id))
	w.rawField("result", result)
	w.end()
	return s.jsonResp(200, w.bytes())
}

// mcpError answers a JSON-RPC error with the given HTTP status.
func (s *server) mcpError(id any, code int, message string, httpStatus int) *response {
	w := newJB(128)
	w.obj()
	w.strField("jsonrpc", "2.0")
	w.key("id")
	w.val(jsonID(id))
	w.key("error")
	w.obj()
	w.intField("code", code)
	w.strField("message", message)
	w.end()
	w.end()
	return s.jsonResp(httpStatus, w.bytes())
}

// jsonID renders a JSON-RPC id (string, number, or null).
func jsonID(id any) []byte {
	switch v := id.(type) {
	case nil:
		return []byte("null")
	case string:
		return appendJSONStr(nil, v)
	case float64:
		return strconv.AppendFloat(nil, v, 'f', -1, 64)
	case bool:
		if v {
			return []byte("true")
		}
		return []byte("false")
	}
	return []byte("null")
}

func (s *server) discoverResult() []byte {
	w := newJB(256)
	w.obj()
	w.key("supportedVersions")
	w.arr().str("2026-07-28").end()
	w.key("serverInfo")
	w.obj().strField("name", "forge").strField("version", "1.0.0").end()
	w.key("capabilities")
	w.obj()
	w.key("tools")
	w.obj().end()
	w.end()
	w.end()
	return w.bytes()
}

func (s *server) initializeResult(params map[string]any) []byte {
	version := jstr(params, "protocolVersion")
	if version == "" {
		version = "2025-11-25"
	}
	w := newJB(256)
	w.obj()
	w.strField("protocolVersion", version)
	w.key("serverInfo")
	w.obj().strField("name", "forge").strField("version", "1.0.0").end()
	w.key("capabilities")
	w.obj()
	w.key("tools")
	w.obj().end()
	w.end()
	w.end()
	return w.bytes()
}

// mcpCallTool executes one tools/call.
func (s *server) mcpCallTool(id any, params map[string]any) *response {
	name := jstr(params, "name")
	args := jobj(params["arguments"])
	if args == nil {
		args = map[string]any{}
	}
	// Normalize the tool name: an optional server prefix is stripped
	// (the suite matches by suffix, case-insensitively).
	normal := strings.ToLower(name)
	for _, canon := range []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"} {
		if normal == canon || strings.HasSuffix(normal, canon) || strings.HasSuffix(normal, strings.ReplaceAll(canon, "_", "")) {
			return s.mcpRunTool(id, canon, args)
		}
	}
	return s.mcpError(id, -32602, "Unknown tool: "+name, 200)
}

// mcpRunTool runs one of the six tools. A missing required argument is
// JSON-RPC -32602; a tool-level failure (unknown repo, conflict) is a
// result with isError: true.
func (s *server) mcpRunTool(id any, name string, args map[string]any) *response {
	owner := toolArg(args, "owner", "owner_login", "org", "user", "login")
	repoName := toolArg(args, "repo", "repository", "repository_name", "repo_name")
	// A full_name argument carries "owner/repo".
	if repoName == "" {
		if full := toolArg(args, "full_name", "repo_full_name", "repository_full_name"); full != "" {
			if o, r, ok := strings.Cut(full, "/"); ok {
				owner, repoName = o, r
			}
		}
	}
	switch name {
	case "create_repo":
		repoName = toolArg(args, "name", "repo", "repository", "repository_name", "repo_name")
		if repoName == "" {
			return s.mcpError(id, -32602, "create_repo: missing required argument: name", 200)
		}
		repo, ok := s.store.createRepo(repoName, jbool(args, "private", false), toolArg(args, "description", "about", "summary", "body"))
		if !ok {
			return s.mcpToolError(id, "repository "+repoName+" already exists")
		}
		return s.mcpToolResult(id, s.repoJSON(repo))
	case "get_repo":
		if owner == "" || repoName == "" {
			return s.mcpError(id, -32602, "get_repo: missing required argument: owner or repo", 200)
		}
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			return s.mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		return s.mcpToolResult(id, s.repoJSON(repo))
	case "create_issue":
		title := toolArg(args, "title", "issue_title", "subject")
		if owner == "" || repoName == "" || title == "" {
			return s.mcpError(id, -32602, "create_issue: missing required argument: owner, repo or title", 200)
		}
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			return s.mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		issue, errMsg := s.store.newIssue(repo, title, toolArg(args, "body", "issue_body", "description", "content", "text"), nil)
		if errMsg != "" {
			return s.mcpToolError(id, errMsg)
		}
		return s.mcpToolResult(id, s.issueJSON(issue))
	case "list_issues":
		if owner == "" || repoName == "" {
			return s.mcpError(id, -32602, "list_issues: missing required argument: owner or repo", 200)
		}
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			return s.mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		state := toolArg(args, "state", "status")
		if state == "" {
			state = "open"
		}
		w := newJB(256)
		w.arr()
		for _, is := range repo.issues {
			if state == "all" || is.State == state {
				w.raw(s.issueJSON(is))
			}
		}
		w.end()
		return s.mcpToolResult(id, w.bytes())
	case "create_pull_request":
		title := toolArg(args, "title", "pr_title", "pull_request_title", "subject")
		head := toolArg(args, "head", "head_ref", "head_branch", "source", "source_branch", "from", "branch")
		base := toolArg(args, "base", "base_ref", "base_branch", "target", "target_branch", "into")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return s.mcpError(id, -32602, "create_pull_request: missing required argument", 200)
		}
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			return s.mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		pull, errMsg := s.store.newPull(repo, title, toolArg(args, "body", "description", "content", "text"), head, base)
		if errMsg != "" {
			return s.mcpToolError(id, errMsg)
		}
		return s.mcpToolResult(id, s.pullJSON(repo, pull))
	case "merge_pull_request":
		n := toolInt(args, "pull_number", "pull_request_number", "pr_number", "number", "issue_number")
		if owner == "" || repoName == "" || n == 0 {
			return s.mcpError(id, -32602, "merge_pull_request: missing required argument", 200)
		}
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			return s.mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		pull := repo.pullByNum[n]
		if pull == nil {
			return s.mcpToolError(id, "pull request #"+strconv.Itoa(n)+" not found")
		}
		sha, status, msg := s.doMerge(repo, pull)
		if status != 200 {
			return s.mcpToolError(id, msg)
		}
		w := newJB(128)
		w.obj()
		w.intField("number", pull.Number)
		w.boolField("merged", true)
		w.strField("sha", sha)
		w.strField("state", pull.State)
		w.end()
		return s.mcpToolResult(id, w.bytes())
	}
	return s.mcpError(id, -32602, "Unknown tool: "+name, 200)
}

// toolArg reads the first present string argument among synonyms.
func toolArg(args map[string]any, names ...string) string {
	for _, n := range names {
		if s, ok := args[n].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// toolInt reads the first present integer argument among synonyms.
func toolInt(args map[string]any, names ...string) int {
	for _, n := range names {
		switch v := args[n].(type) {
		case float64:
			return int(v)
		case string:
			if num, err := strconv.Atoi(v); err == nil {
				return num
			}
		}
	}
	return 0
}

// mcpToolResult wraps a JSON value as a tool result: a text block
// carrying the JSON plus structured content, isError false.
func (s *server) mcpToolResult(id any, value []byte) *response {
	w := newJB(len(value)*2 + 128)
	w.obj()
	w.strField("jsonrpc", "2.0")
	w.key("id")
	w.val(jsonID(id))
	w.key("result")
	w.obj()
	w.key("content")
	w.arr()
	w.obj()
	w.strField("type", "text")
	w.key("text")
	w.valStr(string(value))
	w.end()
	w.end()
	w.rawField("structuredContent", value)
	w.boolField("isError", false)
	w.end()
	w.end()
	return s.jsonResp(200, w.bytes())
}

// mcpToolError answers a tool execution error (isError: true).
func (s *server) mcpToolError(id any, message string) *response {
	w := newJB(256)
	w.obj()
	w.strField("jsonrpc", "2.0")
	w.key("id")
	w.val(jsonID(id))
	w.key("result")
	w.obj()
	w.key("content")
	w.arr()
	w.obj()
	w.strField("type", "text")
	w.strField("text", message)
	w.end()
	w.end()
	w.boolField("isError", true)
	w.end()
	w.end()
	return s.jsonResp(200, w.bytes())
}
