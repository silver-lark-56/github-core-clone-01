// rest.go: the GitHub REST v3 surface under /api/v3 — repositories,
// branches, commits, contents, labels, issues, comments, pull requests.
// Every reply is application/json with a non-empty X-GitHub-Request-Id;
// errors carry {"message": string}; a missing or foreign repository is
// 404, never 403; a request without a bearer token is 401.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// handleREST routes /api/v3/* requests. Auth is required on every route.
func (s *server) handleREST(req *request) *response {
	if resp := s.requireAuth(req); resp != nil {
		return resp
	}
	rest := strings.TrimPrefix(req.path, "/api/v3")
	if rest == "" {
		rest = "/"
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	switch {
	case rest == "/":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.jsonResp(200, []byte(`{"current_user_url":"/api/v3/user"}`),
			[2]string{"X-OAuth-Scopes", "repo, read:org"})
	case rest == "/meta":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
	case rest == "/user":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restGetUser()
	case rest == "/user/repos":
		switch req.method {
		case "POST":
			return s.restCreateRepo(req)
		case "GET":
			return s.restListRepos(req)
		}
		return s.jsonErr(405, "Method not allowed")
	}

	// /repos/{owner}/{name}[/...]
	if strings.HasPrefix(rest, "/repos/") {
		return s.restRepoRoute(req, strings.TrimPrefix(rest, "/repos/"))
	}
	return s.jsonErr(404, "Not Found")
}

func (s *server) restGetUser() *response {
	w := newJB(96)
	w.obj()
	w.intField("id", 1)
	w.strField("login", s.store.viewerLogin)
	w.strField("type", "User")
	w.end()
	return s.jsonResp(200, w.bytes())
}

// --- repositories ---

func (s *server) restCreateRepo(req *request) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	name := jstr(body, "name")
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "/ \t\n") {
		return s.jsonErr(422, "Validation Failed: name is invalid")
	}
	repo, ok := s.store.createRepo(name, jbool(body, "private", false), jstr(body, "description"))
	if !ok {
		return s.jsonErr(422, "Repository already exists")
	}
	return s.jsonResp(201, s.repoJSON(repo))
}

func (s *server) restListRepos(req *request) *response {
	perPage := req.queryInt("per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	page := req.queryInt("page", 1)
	if page < 1 {
		page = 1
	}
	total := len(s.store.repoOrder)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	w := newJB(256)
	w.arr()
	for _, r := range s.store.repoOrder[start:end] {
		w.raw(s.repoJSON(r))
	}
	w.end()
	var extra [][2]string
	if end < total {
		next := "http://" + req.host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
		extra = append(extra, [2]string{"Link", "<" + next + `>; rel="next"`})
	}
	return s.jsonResp(200, w.bytes(), extra...)
}

// repoJSON renders the repository object.
func (s *server) repoJSON(r *Repo) []byte {
	w := newJB(256)
	w.obj()
	w.intField("id", r.DatabaseID)
	w.strField("name", r.Name)
	w.strField("full_name", s.store.viewerLogin+"/"+r.Name)
	w.key("owner")
	w.obj().strField("login", s.store.viewerLogin).intField("id", 1).end()
	w.boolField("private", r.Private)
	w.strField("description", r.Description)
	w.strField("default_branch", r.DefaultBranch)
	w.strField("created_at", r.CreatedAt)
	w.strField("updated_at", s.store.nowISO)
	w.boolField("has_issues", true)
	w.end()
	return w.bytes()
}

// restRepoRoute handles /repos/{owner}/{name} and everything below it.
func (s *server) restRepoRoute(req *request, rest string) *response {
	// Split owner, name, remainder.
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return s.jsonErr(404, "Not Found")
	}
	owner := rest[:i]
	rest = rest[i+1:]
	var name, tail string
	if j := strings.IndexByte(rest, '/'); j < 0 {
		name, tail = rest, ""
	} else {
		name, tail = rest[:j], rest[j+1:]
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		return s.jsonErr(404, "Not Found")
	}
	switch tail {
	case "":
		switch req.method {
		case "GET":
			return s.jsonResp(200, s.repoJSON(repo))
		case "PATCH":
			return s.restPatchRepo(req, repo)
		case "DELETE":
			s.store.deleteRepo(name)
			return s.emptyResp(204)
		}
		return s.jsonErr(405, "Method not allowed")
	case "branches":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restListBranches(repo)
	case "commits":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restListCommits(req, repo)
	case "labels":
		switch req.method {
		case "POST":
			return s.restCreateLabel(req, repo)
		case "GET":
			return s.restListLabels(repo)
		}
		return s.jsonErr(405, "Method not allowed")
	case "issues":
		switch req.method {
		case "POST":
			return s.restCreateIssue(req, repo)
		case "GET":
			return s.restListIssues(req, repo)
		}
		return s.jsonErr(405, "Method not allowed")
	case "pulls":
		switch req.method {
		case "POST":
			return s.restCreatePull(req, repo)
		case "GET":
			return s.restListPulls(req, repo)
		}
		return s.jsonErr(405, "Method not allowed")
	case "git/refs":
		if req.method != "POST" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restCreateRef(req, repo)
	}
	if strings.HasPrefix(tail, "branches/") {
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restGetBranch(repo, strings.TrimPrefix(tail, "branches/"))
	}
	if strings.HasPrefix(tail, "git/refs/") {
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restGetRef(repo, "refs/"+strings.TrimPrefix(tail, "git/refs/"))
	}
	if tail == "contents" || strings.HasPrefix(tail, "contents/") {
		return s.restContents(req, repo, strings.TrimPrefix(tail, "contents"))
	}
	if strings.HasPrefix(tail, "issues/") {
		return s.restIssueRoute(req, repo, strings.TrimPrefix(tail, "issues/"))
	}
	if strings.HasPrefix(tail, "pulls/") {
		return s.restPullRoute(req, repo, strings.TrimPrefix(tail, "pulls/"))
	}
	return s.jsonErr(404, "Not Found")
}

func (s *server) restPatchRepo(req *request, repo *Repo) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	if desc, ok := jstrOK(body, "description"); ok {
		repo.Description = desc
	}
	if priv, ok := body["private"].(bool); ok {
		repo.Private = priv
	}
	return s.jsonResp(200, s.repoJSON(repo))
}

func (s *server) restListBranches(repo *Repo) *response {
	w := newJB(128)
	w.arr()
	for _, name := range repo.sortedRefNames() {
		if !strings.HasPrefix(name, "refs/heads/") {
			continue
		}
		w.obj()
		w.strField("name", strings.TrimPrefix(name, "refs/heads/"))
		w.key("commit")
		w.obj().strField("sha", repo.refs[name].hex()).end()
		w.end()
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

func (s *server) restGetBranch(repo *Repo, branch string) *response {
	oid, ok := repo.refTip("refs/heads/" + branch)
	if !ok {
		return s.jsonErr(404, "Branch not found")
	}
	w := newJB(128)
	w.obj()
	w.strField("name", branch)
	w.key("commit")
	w.obj().strField("sha", oid.hex()).end()
	w.end()
	return s.jsonResp(200, w.bytes())
}

// resolveRef resolves a branch name or commit sha to a commit oid.
func (s *server) resolveRef(repo *Repo, ref string) (Oid, bool) {
	if oid, ok := repo.refTip("refs/heads/" + ref); ok {
		return oid, true
	}
	if oid, ok := oidFromHex(ref); ok {
		if o := repo.objects[oid]; o != nil && o.typ == objCommit {
			return oid, true
		}
	}
	return ZeroOid, false
}

func (s *server) restListCommits(req *request, repo *Repo) *response {
	var tip Oid
	var ok bool
	if sha, present := req.queryValue("sha"); present && sha != "" {
		tip, ok = s.resolveRef(repo, sha)
		if !ok {
			return s.jsonErr(404, "No commit found for SHA: "+sha)
		}
	} else {
		tip, ok = repo.defaultTip()
		if !ok {
			return s.jsonResp(200, []byte("[]"))
		}
	}
	history := repo.commitHistory(tip, 1000)
	w := newJB(256)
	w.arr()
	for _, oid := range history {
		w.raw(s.commitJSON(repo, oid))
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

func (s *server) commitJSON(repo *Repo, oid Oid) []byte {
	o := repo.objects[oid]
	msg := ""
	if o != nil {
		msg = commitMessage(o.data)
	}
	login := s.store.viewerLogin
	w := newJB(256)
	w.obj()
	w.strField("sha", oid.hex())
	w.key("commit")
	w.obj()
	w.strField("message", msg)
	w.key("author")
	w.obj().strField("name", login).strField("email", login+"@forge.local").strField("date", s.store.nowISO).end()
	w.key("committer")
	w.obj().strField("name", login).strField("email", login+"@forge.local").strField("date", s.store.nowISO).end()
	w.end()
	w.key("author")
	w.obj().strField("login", login).end()
	w.key("committer")
	w.obj().strField("login", login).end()
	w.end()
	return w.bytes()
}

func (s *server) restCreateRef(req *request, repo *Repo) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	ref := jstr(body, "ref")
	sha := jstr(body, "sha")
	if !strings.HasPrefix(ref, "refs/") || ref == "" {
		return s.jsonErr(422, "Validation Failed: ref must start with refs/")
	}
	oid, ok := oidFromHex(sha)
	if !ok {
		return s.jsonErr(422, "Validation Failed: sha is invalid")
	}
	obj := repo.objects[oid]
	if obj == nil {
		return s.jsonErr(422, "Validation Failed: sha does not exist")
	}
	if _, exists := repo.refs[ref]; exists {
		return s.jsonErr(422, "Reference already exists")
	}
	repo.setRef(ref, oid)
	w := newJB(128)
	w.obj()
	w.strField("ref", ref)
	w.key("object")
	w.obj().strField("sha", oid.hex()).strField("type", objTypeName(obj.typ)).end()
	w.end()
	return s.jsonResp(201, w.bytes())
}

func (s *server) restGetRef(repo *Repo, ref string) *response {
	oid, ok := repo.refTip(ref)
	if !ok {
		return s.jsonErr(404, "Reference not found")
	}
	obj := repo.objects[oid]
	typ := ""
	if obj != nil {
		typ = objTypeName(obj.typ)
	}
	w := newJB(128)
	w.obj()
	w.strField("ref", ref)
	w.key("object")
	w.obj().strField("sha", oid.hex()).strField("type", typ).end()
	w.end()
	return s.jsonResp(200, w.bytes())
}

// --- contents ---

// restContents handles PUT/GET/DELETE /contents[/{path...}].
func (s *server) restContents(req *request, repo *Repo, tail string) *response {
	path := strings.TrimPrefix(tail, "/")
	switch req.method {
	case "PUT":
		return s.restPutContents(req, repo, path)
	case "GET":
		return s.restGetContents(req, repo, path)
	case "DELETE":
		return s.restDeleteContents(req, repo, path)
	}
	return s.jsonErr(405, "Method not allowed")
}

// splitPath splits a repo-relative path into clean segments.
func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (s *server) restPutContents(req *request, repo *Repo, path string) *response {
	segments := splitPath(path)
	if len(segments) == 0 {
		return s.jsonErr(422, "Validation Failed: path is empty")
	}
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	contentB64 := jstr(body, "content")
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		// Tolerate base64 without padding or with newlines.
		clean := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, contentB64)
		content, err = base64.StdEncoding.DecodeString(clean)
		if err != nil {
			content, err = base64.RawStdEncoding.DecodeString(clean)
			if err != nil {
				return s.jsonErr(422, "Validation Failed: content is not valid base64")
			}
		}
	}
	message := jstr(body, "message")
	branch := jstr(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, branchExists := repo.refTip(refName)
	var parentTree Oid
	var parents []Oid
	if branchExists {
		parents = []Oid{tip}
		parentTree, _ = repo.commitTree(tip)
	} else {
		// A new branch starts from the default branch tip (or unborn).
		if defTip, ok := repo.defaultTip(); ok && branch != repo.DefaultBranch {
			parents = []Oid{defTip}
			parentTree, _ = repo.commitTree(defTip)
		}
	}
	existing := ZeroOid
	if parentTree != ZeroOid || len(parents) > 0 {
		existing = repo.findTreePath(parentTree, segments)
	}
	// A sha pins the update: it must match the current blob.
	if sha, ok := jstrOK(body, "sha"); ok && sha != "" {
		want, good := oidFromHex(sha)
		if !good || existing.isZero() || existing != want {
			return s.jsonErr(409, "sha does not match the current file")
		}
	}
	blobOid := repo.putObject(objBlob, content)
	newTree := repo.setTreePath(parentTree, segments, blobOid)
	login := s.store.viewerLogin
	commitOid := repo.putObject(objCommit, buildCommit(newTree, parents, login, login+"@forge.local", s.store.nowUnixSec, message))
	repo.setRef(refName, commitOid)
	status := 201
	if !existing.isZero() {
		status = 200
	}
	w := newJB(512)
	w.obj()
	w.key("content")
	w.obj()
	w.strField("type", "file")
	w.strField("encoding", "base64")
	w.strField("name", segments[len(segments)-1])
	w.strField("path", path)
	w.strField("sha", blobOid.hex())
	w.intField("size", len(content))
	w.strField("content", contentB64)
	w.end()
	w.key("commit")
	w.obj()
	w.strField("sha", commitOid.hex())
	w.strField("message", message)
	w.end()
	w.end()
	return s.jsonResp(status, w.bytes())
}

func (s *server) restGetContents(req *request, repo *Repo, path string) *response {
	ref, _ := req.queryValue("ref")
	var tip Oid
	var ok bool
	if ref == "" {
		tip, ok = repo.defaultTip()
	} else {
		tip, ok = s.resolveRef(repo, ref)
	}
	if !ok {
		return s.jsonErr(404, "Not Found")
	}
	treeOid, ok := repo.commitTree(tip)
	if !ok {
		return s.jsonErr(404, "Not Found")
	}
	segments := splitPath(path)
	if len(segments) == 0 {
		// The repository root is a directory listing.
		return s.jsonResp(200, s.dirJSON(repo, treeOid, ""))
	}
	// File or directory?
	if oid := repo.findTreePath(treeOid, segments); !oid.isZero() {
		obj := repo.objects[oid]
		if obj == nil {
			return s.jsonErr(404, "Not Found")
		}
		w := newJB(512)
		w.obj()
		w.strField("type", "file")
		w.strField("encoding", "base64")
		w.strField("name", segments[len(segments)-1])
		w.strField("path", path)
		w.strField("sha", oid.hex())
		w.intField("size", len(obj.data))
		w.strField("content", base64.StdEncoding.EncodeToString(obj.data))
		w.end()
		return s.jsonResp(200, w.bytes())
	}
	if dirOid, ok := repo.findTreeDir(treeOid, segments); ok {
		return s.jsonResp(200, s.dirJSON(repo, dirOid, path))
	}
	return s.jsonErr(404, "Not Found")
}

func (s *server) dirJSON(repo *Repo, treeOid Oid, prefix string) []byte {
	entries := repo.treeEntriesOf(treeOid)
	w := newJB(256)
	w.arr()
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		w.obj()
		if e.Mode == "40000" {
			w.strField("type", "dir")
		} else {
			w.strField("type", "file")
		}
		w.strField("name", e.Name)
		w.strField("path", path)
		w.strField("sha", e.Oid.hex())
		if e.Mode != "40000" {
			size := 0
			if o := repo.objects[e.Oid]; o != nil {
				size = len(o.data)
			}
			w.intField("size", size)
		}
		w.end()
	}
	w.end()
	return w.bytes()
}

func (s *server) restDeleteContents(req *request, repo *Repo, path string) *response {
	segments := splitPath(path)
	if len(segments) == 0 {
		return s.jsonErr(422, "Validation Failed: path is empty")
	}
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	message := jstr(body, "message")
	branch := jstr(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, ok := repo.refTip(refName)
	if !ok {
		return s.jsonErr(404, "Not Found")
	}
	treeOid, ok := repo.commitTree(tip)
	if !ok {
		return s.jsonErr(404, "Not Found")
	}
	existing := repo.findTreePath(treeOid, segments)
	if existing.isZero() {
		return s.jsonErr(404, "Not Found")
	}
	if sha, present := jstrOK(body, "sha"); present && sha != "" {
		want, good := oidFromHex(sha)
		if !good || want != existing {
			return s.jsonErr(409, "sha does not match the current file")
		}
	}
	newTree, ok := repo.deleteTreePath(treeOid, segments)
	if !ok {
		return s.jsonErr(404, "Not Found")
	}
	login := s.store.viewerLogin
	commitOid := repo.putObject(objCommit, buildCommit(newTree, []Oid{tip}, login, login+"@forge.local", s.store.nowUnixSec, message))
	repo.setRef(refName, commitOid)
	w := newJB(128)
	w.obj()
	w.key("commit")
	w.obj().strField("sha", commitOid.hex()).strField("message", message).end()
	w.end()
	return s.jsonResp(200, w.bytes())
}

// --- labels ---

func (s *server) restCreateLabel(req *request, repo *Repo) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	name := jstr(body, "name")
	if name == "" {
		return s.jsonErr(422, "Validation Failed: name is required")
	}
	if l, ok := repo.labels[name]; ok {
		return s.jsonResp(201, s.labelJSON(l))
	}
	l := &Label{Name: name, Color: jstr(body, "color")}
	repo.labels[name] = l
	repo.labelOrder = append(repo.labelOrder, l)
	return s.jsonResp(201, s.labelJSON(l))
}

func (s *server) restListLabels(repo *Repo) *response {
	w := newJB(128)
	w.arr()
	for _, l := range repo.labelOrder {
		w.raw(s.labelJSON(l))
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

func (s *server) labelJSON(l *Label) []byte {
	w := newJB(64)
	w.obj()
	w.strField("name", l.Name)
	w.strField("color", l.Color)
	w.end()
	return w.bytes()
}

// --- issues ---

func (s *server) restCreateIssue(req *request, repo *Repo) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	title := jstr(body, "title")
	if title == "" {
		return s.jsonErr(422, "Validation Failed: title is required")
	}
	issue, errMsg := s.store.newIssue(repo, title, jstr(body, "body"), jstrs(body, "labels"))
	if errMsg != "" {
		return s.jsonErr(422, errMsg)
	}
	return s.jsonResp(201, s.issueJSON(issue))
}

// newIssue creates an issue; errMsg is non-empty when a label is unknown.
func (s *Store) newIssue(repo *Repo, title, body string, labelNames []string) (*Issue, string) {
	var labels []*Label
	for _, name := range labelNames {
		l, ok := repo.labels[name]
		if !ok {
			return nil, "Validation Failed: label " + name + " does not exist"
		}
		labels = append(labels, l)
	}
	issue := &Issue{
		ID:        s.nextRestID,
		Number:    repo.nextNum,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		CreatedAt: s.nowISO,
		UpdatedAt: s.nowISO,
	}
	s.nextRestID++
	repo.nextNum++
	repo.issues = append(repo.issues, issue)
	repo.issueByNum[issue.Number] = issue
	return issue, ""
}

func (s *server) restListIssues(req *request, repo *Repo) *response {
	state, _ := req.queryValue("state")
	if state == "" {
		state = "open"
	}
	direction, _ := req.queryValue("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := req.queryInt("per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	page := req.queryInt("page", 1)
	if page < 1 {
		page = 1
	}
	// Filter (state), then order by creation (number).
	var filtered []*Issue
	for _, is := range repo.issues {
		if state == "all" || is.State == state {
			filtered = append(filtered, is)
		}
	}
	if direction == "desc" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	start := (page - 1) * perPage
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + perPage
	if end > len(filtered) {
		end = len(filtered)
	}
	w := newJB(256)
	w.arr()
	for _, is := range filtered[start:end] {
		w.raw(s.issueJSON(is))
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

// issueJSON renders an issue, memoized per issue (invalidated on PATCH).
func (s *server) issueJSON(is *Issue) []byte {
	if is.jsonCache != nil {
		return is.jsonCache
	}
	w := newJB(256)
	w.obj()
	w.intField("id", is.ID)
	w.intField("number", is.Number)
	w.strField("title", is.Title)
	w.strField("body", is.Body)
	w.strField("state", is.State)
	w.key("labels")
	w.arr()
	for _, l := range is.Labels {
		w.raw(s.labelJSON(l))
	}
	w.end()
	w.key("user")
	w.obj().strField("login", s.store.viewerLogin).end()
	w.strField("created_at", is.CreatedAt)
	w.strField("updated_at", is.UpdatedAt)
	w.end()
	is.jsonCache = w.bytes()
	return is.jsonCache
}

func (s *server) restIssueRoute(req *request, repo *Repo, tail string) *response {
	// tail: "{n}" or "{n}/comments"
	numStr, rest, _ := strings.Cut(tail, "/")
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.jsonErr(404, "Not Found")
	}
	issue := repo.issueByNum[n]
	if issue == nil {
		return s.jsonErr(404, "Not Found")
	}
	switch rest {
	case "":
		switch req.method {
		case "GET":
			return s.jsonResp(200, s.issueJSON(issue))
		case "PATCH":
			return s.restPatchIssue(req, repo, issue)
		}
		return s.jsonErr(405, "Method not allowed")
	case "comments":
		switch req.method {
		case "POST":
			return s.restCreateComment(req, repo, issue)
		case "GET":
			return s.restListComments(issue)
		}
		return s.jsonErr(405, "Method not allowed")
	}
	return s.jsonErr(404, "Not Found")
}

func (s *server) restPatchIssue(req *request, repo *Repo, issue *Issue) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	if state, ok := jstrOK(body, "state"); ok && (state == "open" || state == "closed") {
		issue.State = state
	}
	if title, ok := jstrOK(body, "title"); ok {
		issue.Title = title
	}
	if b, ok := jstrOK(body, "body"); ok {
		issue.Body = b
	}
	issue.UpdatedAt = s.store.nowISO
	issue.jsonCache = nil
	return s.jsonResp(200, s.issueJSON(issue))
}

func (s *server) restCreateComment(req *request, repo *Repo, issue *Issue) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	c := &Comment{
		ID:        s.store.nextCommentID,
		IssueNum:  issue.Number,
		Body:      jstr(body, "body"),
		CreatedAt: s.store.nowISO,
	}
	s.store.nextCommentID++
	issue.Comments = append(issue.Comments, c)
	return s.jsonResp(201, s.commentJSON(c))
}

func (s *server) restListComments(issue *Issue) *response {
	w := newJB(128)
	w.arr()
	for _, c := range issue.Comments {
		w.raw(s.commentJSON(c))
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

func (s *server) commentJSON(c *Comment) []byte {
	w := newJB(128)
	w.obj()
	w.intField("id", c.ID)
	w.strField("body", c.Body)
	w.key("user")
	w.obj().strField("login", s.store.viewerLogin).end()
	w.strField("created_at", c.CreatedAt)
	w.strField("updated_at", c.CreatedAt)
	w.end()
	return w.bytes()
}

// --- pull requests ---

func (s *server) restCreatePull(req *request, repo *Repo) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	title := jstr(body, "title")
	head := jstr(body, "head")
	base := jstr(body, "base")
	if title == "" || head == "" || base == "" {
		return s.jsonErr(422, "Validation Failed: title, head and base are required")
	}
	pull, errMsg := s.store.newPull(repo, title, jstr(body, "body"), head, base)
	if errMsg != "" {
		return s.jsonErr(422, errMsg)
	}
	return s.jsonResp(201, s.pullJSON(repo, pull))
}

// newPull opens a pull request; errMsg is non-empty on bad branches or
// a duplicate open pull request for the same head and base.
func (s *Store) newPull(repo *Repo, title, body, head, base string) (*Pull, string) {
	headOid, ok := repo.refTip("refs/heads/" + head)
	if !ok {
		return nil, "Validation Failed: head branch does not exist"
	}
	baseOid, ok := repo.refTip("refs/heads/" + base)
	if !ok {
		return nil, "Validation Failed: base branch does not exist"
	}
	for _, p := range repo.pulls {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
			return nil, "Validation Failed: a pull request already exists for " + head
		}
	}
	pull := &Pull{
		ID:        s.nextRestID,
		Number:    repo.nextNum,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		BaseRef:   base,
		HeadSHA:   headOid.hex(),
		BaseSHA:   baseOid.hex(),
		CreatedAt: s.nowISO,
		UpdatedAt: s.nowISO,
	}
	s.nextRestID++
	repo.nextNum++
	repo.pulls = append(repo.pulls, pull)
	repo.pullByNum[pull.Number] = pull
	return pull, ""
}

func (s *server) restListPulls(req *request, repo *Repo) *response {
	state, _ := req.queryValue("state")
	if state == "" {
		state = "open"
	}
	perPage := req.queryInt("per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	page := req.queryInt("page", 1)
	if page < 1 {
		page = 1
	}
	var filtered []*Pull
	for _, p := range repo.pulls {
		if state == "all" || p.State == state {
			filtered = append(filtered, p)
		}
	}
	start := (page - 1) * perPage
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + perPage
	if end > len(filtered) {
		end = len(filtered)
	}
	w := newJB(256)
	w.arr()
	for _, p := range filtered[start:end] {
		w.raw(s.pullJSON(repo, p))
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

// pullJSON renders a pull request.
func (s *server) pullJSON(repo *Repo, p *Pull) []byte {
	login := s.store.viewerLogin
	w := newJB(512)
	w.obj()
	w.intField("id", p.ID)
	w.intField("number", p.Number)
	w.strField("title", p.Title)
	w.strField("body", p.Body)
	w.strField("state", p.State)
	w.boolField("merged", p.Merged)
	w.boolField("mergeable", s.pullMergeable(repo, p))
	if p.MergedAt == "" {
		w.nullField("merged_at")
	} else {
		w.strField("merged_at", p.MergedAt)
	}
	if p.MergeCommit == "" {
		w.nullField("merge_commit_sha")
	} else {
		w.strField("merge_commit_sha", p.MergeCommit)
	}
	w.key("head")
	w.obj()
	w.strField("ref", p.HeadRef)
	w.strField("sha", p.HeadSHA)
	w.strField("label", login+":"+p.HeadRef)
	w.end()
	w.key("base")
	w.obj()
	w.strField("ref", p.BaseRef)
	w.strField("sha", p.BaseSHA)
	w.strField("label", login+":"+p.BaseRef)
	w.end()
	w.key("user")
	w.obj().strField("login", login).end()
	w.strField("created_at", p.CreatedAt)
	w.strField("updated_at", p.UpdatedAt)
	w.end()
	return w.bytes()
}

func (s *server) restPullRoute(req *request, repo *Repo, tail string) *response {
	// tail: "{n}", "{n}/files" or "{n}/merge"
	numStr, rest, _ := strings.Cut(tail, "/")
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return s.jsonErr(404, "Not Found")
	}
	pull := repo.pullByNum[n]
	if pull == nil {
		return s.jsonErr(404, "Not Found")
	}
	switch rest {
	case "":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.jsonResp(200, s.pullJSON(repo, pull))
	case "files":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restPullFiles(repo, pull)
	case "merge":
		if req.method != "PUT" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.restMergePull(req, repo, pull)
	}
	return s.jsonErr(404, "Not Found")
}

func (s *server) restPullFiles(repo *Repo, pull *Pull) *response {
	files := s.pullFiles(repo, pull)
	w := newJB(256)
	w.arr()
	for _, f := range files {
		w.obj()
		w.strField("filename", f.path)
		w.strField("status", f.status)
		w.intField("additions", f.additions)
		w.intField("deletions", f.deletions)
		w.intField("changes", f.additions+f.deletions)
		w.end()
	}
	w.end()
	return s.jsonResp(200, w.bytes())
}

func (s *server) restMergePull(req *request, repo *Repo, pull *Pull) *response {
	body, errResp := s.parseJSONBody(req)
	if errResp != nil {
		return errResp
	}
	_ = body // merge_method is accepted (squash); nothing else is required
	sha, status, msg := s.doMerge(repo, pull)
	if status != 200 {
		return s.jsonErr(status, msg)
	}
	w := newJB(128)
	w.obj()
	w.strField("sha", sha)
	w.boolField("merged", true)
	w.strField("message", "Pull Request successfully merged")
	w.end()
	return s.jsonResp(200, w.bytes())
}
