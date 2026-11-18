// graphql.go: a small GraphQL parser and executor for the fixed set of
// documents the suite sends (gh's queries and mutations, plus the
// introspection probes). It parses real GraphQL — operations, fragments,
// aliases, variables — and executes against the same store REST and MCP
// use, so every surface reads the same objects.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// --- tokenizer ---

type tokKind int

const (
	tName tokKind = iota
	tInt
	tFloat
	tString
	tPunct
	tEOF
)

type token struct {
	kind tokKind
	text string
}

func tokenize(src string) ([]token, bool) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			out = append(out, token{tPunct, "..."})
			i += 3
		case c == '!' || c == '$' || c == '(' || c == ')' || c == ':' || c == '=' || c == '@' || c == '[' || c == ']' || c == '{' || c == '}' || c == '|' || c == '&':
			out = append(out, token{tPunct, string(c)})
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			ok := false
			for j < len(src) {
				if src[j] == '"' {
					ok = true
					break
				}
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case 'r':
						b.WriteByte('\r')
					case 'b':
						b.WriteByte('\b')
					case 'f':
						b.WriteByte('\f')
					case '/':
						b.WriteByte('/')
					case '\\':
						b.WriteByte('\\')
					case '"':
						b.WriteByte('"')
					case 'u':
						if j+4 < len(src) {
							if v, err := strconv.ParseUint(src[j+1:j+5], 16, 32); err == nil {
								b.WriteRune(rune(v))
								j += 4
							}
						}
					}
					j++
					continue
				}
				b.WriteByte(src[j])
				j++
			}
			if !ok {
				return nil, false
			}
			out = append(out, token{tString, b.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if isFloat {
				out = append(out, token{tFloat, src[i:j]})
			} else {
				out = append(out, token{tInt, src[i:j]})
			}
			i = j
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
			j := i
			for j < len(src) {
				d := src[j]
				if d == '_' || (d >= 'A' && d <= 'Z') || (d >= 'a' && d <= 'z') || (d >= '0' && d <= '9') {
					j++
					continue
				}
				break
			}
			out = append(out, token{tName, src[i:j]})
			i = j
		default:
			return nil, false
		}
	}
	out = append(out, token{tEOF, ""})
	return out, true
}

// --- AST ---

type gqlValue struct {
	kind string // var,int,float,string,bool,null,enum,list,object
	s    string
	f    float64
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sels  []gqlSelection
}

type gqlSelection struct {
	kind     string // field, spread, inline
	field    *gqlField
	fragName string
	typeCond string
	sels     []gqlSelection
}

type gqlOperation struct {
	opType string // query | mutation
	name   string
	sels   []gqlSelection
}

type gqlDocument struct {
	ops   []*gqlOperation
	frags map[string][]gqlSelection
}

type gqlParser struct {
	toks []token
	i    int
}

func (p *gqlParser) peek() token  { return p.toks[p.i] }
func (p *gqlParser) next() token  { t := p.toks[p.i]; p.i++; return t }
func (p *gqlParser) eof() bool    { return p.toks[p.i].kind == tEOF }
func (p *gqlParser) isPunct(s string) bool {
	t := p.peek()
	return t.kind == tPunct && t.text == s
}
func (p *gqlParser) eatPunct(s string) bool {
	if p.isPunct(s) {
		p.i++
		return true
	}
	return false
}
func (p *gqlParser) isName(s string) bool {
	t := p.peek()
	return t.kind == tName && t.text == s
}

func parseGraphQL(src string) (*gqlDocument, bool) {
	toks, ok := tokenize(src)
	if !ok {
		return nil, false
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDocument{frags: make(map[string][]gqlSelection)}
	for !p.eof() {
		if p.isPunct("{") {
			sels, ok := p.selectionSet()
			if !ok {
				return nil, false
			}
			doc.ops = append(doc.ops, &gqlOperation{opType: "query", sels: sels})
			continue
		}
		if p.isName("fragment") {
			p.next()
			nameTok := p.next()
			if nameTok.kind != tName {
				return nil, false
			}
			if !p.isName("on") {
				return nil, false
			}
			p.next()
			if p.next().kind != tName { // type condition
				return nil, false
			}
			// Directives on the fragment definition: skip.
			if !p.skipDirectives() {
				return nil, false
			}
			sels, ok := p.selectionSet()
			if !ok {
				return nil, false
			}
			doc.frags[nameTok.text] = sels
			continue
		}
		opTok := p.next()
		if opTok.kind != tName || (opTok.text != "query" && opTok.text != "mutation" && opTok.text != "subscription") {
			return nil, false
		}
		op := &gqlOperation{opType: opTok.text}
		if p.peek().kind == tName {
			op.name = p.next().text
		}
		if p.isPunct("(") {
			if !p.skipVarDefs() {
				return nil, false
			}
		}
		if !p.skipDirectives() {
			return nil, false
		}
		sels, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		op.sels = sels
		doc.ops = append(doc.ops, op)
	}
	if len(doc.ops) == 0 {
		return nil, false
	}
	return doc, true
}

// skipVarDefs skips a parenthesised variable-definition list.
func (p *gqlParser) skipVarDefs() bool {
	depth := 0
	for !p.eof() {
		t := p.next()
		if t.kind == tPunct {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
				if depth == 0 {
					return true
				}
			}
		}
	}
	return false
}

// skipDirectives skips @name(args) directives.
func (p *gqlParser) skipDirectives() bool {
	for p.isPunct("@") {
		p.next()
		if p.next().kind != tName {
			return false
		}
		if p.isPunct("(") {
			if !p.skipArgs() {
				return false
			}
		}
	}
	return true
}

// skipArgs skips a parenthesised argument list (used for directives).
func (p *gqlParser) skipArgs() bool { return p.skipVarDefs() }

func (p *gqlParser) selectionSet() ([]gqlSelection, bool) {
	if !p.eatPunct("{") {
		return nil, false
	}
	var sels []gqlSelection
	for !p.eof() && !p.isPunct("}") {
		sel, ok := p.selection()
		if !ok {
			return nil, false
		}
		sels = append(sels, sel)
	}
	if !p.eatPunct("}") {
		return nil, false
	}
	return sels, true
}

func (p *gqlParser) selection() (gqlSelection, bool) {
	if p.isPunct("...") {
		p.next()
		if p.isName("on") {
			p.next()
			typeTok := p.next()
			if typeTok.kind != tName {
				return gqlSelection{}, false
			}
			if !p.skipDirectives() {
				return gqlSelection{}, false
			}
			sels, ok := p.selectionSet()
			if !ok {
				return gqlSelection{}, false
			}
			return gqlSelection{kind: "inline", typeCond: typeTok.text, sels: sels}, true
		}
		if p.peek().kind == tName {
			name := p.next().text
			if !p.skipDirectives() {
				return gqlSelection{}, false
			}
			return gqlSelection{kind: "spread", fragName: name}, true
		}
		if p.isPunct("{") {
			sels, ok := p.selectionSet()
			if !ok {
				return gqlSelection{}, false
			}
			return gqlSelection{kind: "inline", sels: sels}, true
		}
		return gqlSelection{}, false
	}
	nameTok := p.next()
	if nameTok.kind != tName {
		return gqlSelection{}, false
	}
	f := &gqlField{name: nameTok.text, alias: nameTok.text}
	if p.eatPunct(":") {
		realTok := p.next()
		if realTok.kind != tName {
			return gqlSelection{}, false
		}
		f.alias = nameTok.text
		f.name = realTok.text
	}
	if p.isPunct("(") {
		args, ok := p.arguments()
		if !ok {
			return gqlSelection{}, false
		}
		f.args = args
	}
	if !p.skipDirectives() {
		return gqlSelection{}, false
	}
	if p.isPunct("{") {
		sels, ok := p.selectionSet()
		if !ok {
			return gqlSelection{}, false
		}
		f.sels = sels
	}
	return gqlSelection{kind: "field", field: f}, true
}

func (p *gqlParser) arguments() (map[string]*gqlValue, bool) {
	if !p.eatPunct("(") {
		return nil, false
	}
	args := make(map[string]*gqlValue)
	for !p.eof() && !p.isPunct(")") {
		nameTok := p.next()
		if nameTok.kind != tName {
			return nil, false
		}
		if !p.eatPunct(":") {
			return nil, false
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		args[nameTok.text] = v
	}
	if !p.eatPunct(")") {
		return nil, false
	}
	return args, true
}

func (p *gqlParser) value() (*gqlValue, bool) {
	t := p.next()
	switch t.kind {
	case tInt:
		f, _ := strconv.ParseFloat(t.text, 64)
		return &gqlValue{kind: "int", s: t.text, f: f}, true
	case tFloat:
		f, _ := strconv.ParseFloat(t.text, 64)
		return &gqlValue{kind: "float", s: t.text, f: f}, true
	case tString:
		return &gqlValue{kind: "string", s: t.text}, true
	case tName:
		switch t.text {
		case "true":
			return &gqlValue{kind: "bool", s: "true"}, true
		case "false":
			return &gqlValue{kind: "bool", s: "false"}, true
		case "null":
			return &gqlValue{kind: "null"}, true
		}
		return &gqlValue{kind: "enum", s: t.text}, true
	case tPunct:
		switch t.text {
		case "$":
			nameTok := p.next()
			if nameTok.kind != tName {
				return nil, false
			}
			return &gqlValue{kind: "var", s: nameTok.text}, true
		case "[":
			v := &gqlValue{kind: "list"}
			for !p.eof() && !p.isPunct("]") {
				item, ok := p.value()
				if !ok {
					return nil, false
				}
				v.list = append(v.list, item)
			}
			if !p.eatPunct("]") {
				return nil, false
			}
			return v, true
		case "{":
			v := &gqlValue{kind: "object", obj: make(map[string]*gqlValue)}
			for !p.eof() && !p.isPunct("}") {
				nameTok := p.next()
				if nameTok.kind != tName {
					return nil, false
				}
				if !p.eatPunct(":") {
					return nil, false
				}
				item, ok := p.value()
				if !ok {
					return nil, false
				}
				v.obj[nameTok.text] = item
			}
			if !p.eatPunct("}") {
				return nil, false
			}
			return v, true
		}
	}
	return nil, false
}

// resolveValue turns a parsed argument value into a plain Go value,
// substituting $variables from vars.
func resolveValue(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "var":
		if vars == nil {
			return nil
		}
		return vars[v.s]
	case "int", "float":
		return v.f
	case "string", "enum":
		return v.s
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, len(v.list))
		for i, item := range v.list {
			out[i] = resolveValue(item, vars)
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for k, item := range v.obj {
			out[k] = resolveValue(item, vars)
		}
		return out
	}
	return nil
}

func argStr(f *gqlField, name string, vars map[string]any) string {
	v, ok := f.args[name]
	if !ok {
		return ""
	}
	if s, ok := resolveValue(v, vars).(string); ok {
		return s
	}
	return ""
}

func argInt(f *gqlField, name string, vars map[string]any) (int, bool) {
	v, ok := f.args[name]
	if !ok {
		return 0, false
	}
	if n, ok := resolveValue(v, vars).(float64); ok {
		return int(n), true
	}
	return 0, false
}

func argObj(f *gqlField, name string, vars map[string]any) map[string]any {
	v, ok := f.args[name]
	if !ok {
		return nil
	}
	return jobj(resolveValue(v, vars))
}

// --- executor ---

// gqlVal is a resolved value with its runtime type name.
type gqlVal struct {
	typ string
	v   any
}

func scalarVal(v any) gqlVal { return gqlVal{typ: "scalar", v: v} }

type gqlCtx struct {
	s    *server
	doc  *gqlDocument
	vars map[string]any
}

// handleGraphQL serves POST /api/graphql (single operation or a batch
// array of operations).
func (s *server) handleGraphQL(req *request) *response {
	if resp := s.requireAuth(req); resp != nil {
		return resp
	}
	if len(req.body) == 0 {
		return s.jsonErr(400, "Invalid JSON body")
	}
	parsed, err := ParseJSON(req.body)
	if err != nil {
		return s.jsonErr(400, "Invalid JSON body")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if arr, ok := parsed.([]any); ok {
		w := newJB(512)
		w.arr()
		for _, item := range arr {
			w.raw(s.execGraphQLOp(jobj(item)))
		}
		w.end()
		return s.jsonResp(200, w.bytes())
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return s.jsonErr(400, "Invalid GraphQL body")
	}
	return s.jsonResp(200, s.execGraphQLOp(m))
}

// execGraphQLOp runs one {query, variables, operationName} operation.
func (s *server) execGraphQLOp(m map[string]any) []byte {
	query := jstr(m, "query")
	vars := jobj(m["variables"])
	opName := jstr(m, "operationName")
	doc, ok := parseGraphQL(query)
	if !ok {
		return gqlErrorJSON("Syntax Error: the query could not be parsed")
	}
	var op *gqlOperation
	if opName != "" {
		for _, cand := range doc.ops {
			if cand.name == opName {
				op = cand
				break
			}
		}
		if op == nil {
			return gqlErrorJSON("Unknown operation named \"" + opName + "\"")
		}
	} else if len(doc.ops) == 1 {
		op = doc.ops[0]
	} else {
		return gqlErrorJSON("Must provide operation name if query contains multiple operations")
	}
	ctx := &gqlCtx{s: s, doc: doc, vars: vars}
	w := newJB(512)
	w.obj()
	w.key("data")
	root := gqlVal{typ: "Query"}
	if op.opType == "mutation" {
		root = gqlVal{typ: "Mutation"}
	}
	data := newJB(512)
	data.obj()
	if err := s.execSelections(data, root, op.sels, ctx); err != nil {
		return gqlErrorJSON(err.Error())
	}
	data.end()
	w.val(data.bytes()) // the "data" key was already written
	w.end()
	return w.bytes()
}

func gqlErrorJSON(msg string) []byte {
	w := newJB(128)
	w.obj()
	w.nullField("data")
	w.key("errors")
	w.arr()
	w.obj().strField("message", msg).end()
	w.end()
	w.end()
	return w.bytes()
}

type gqlError string

func (e gqlError) Error() string { return string(e) }

func unknownFieldErr(field, typ string) error {
	return gqlError("Cannot query field \"" + field + "\" on type \"" + typ + "\".")
}

// execSelections writes the selected fields of parent into w (an open
// object). Fragment spreads and inline fragments merge by type.
func (s *server) execSelections(w *jb, parent gqlVal, sels []gqlSelection, ctx *gqlCtx) error {
	for _, sel := range sels {
		switch sel.kind {
		case "spread":
			body, ok := ctx.doc.frags[sel.fragName]
			if !ok {
				return gqlError("Unknown fragment \"" + sel.fragName + "\"")
			}
			if err := s.execSelections(w, parent, body, ctx); err != nil {
				return err
			}
		case "inline":
			if sel.typeCond != "" && sel.typeCond != parent.typ {
				continue
			}
			if err := s.execSelections(w, parent, sel.sels, ctx); err != nil {
				return err
			}
		case "field":
			f := sel.field
			val, err := s.resolveField(parent, f, ctx)
			if err != nil {
				return err
			}
			w.key(f.alias)
			if err := s.writeGQLValue(w, val, f.sels, ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeGQLValue writes a resolved value: scalars inline, objects via
// their selection set, lists element by element.
func (s *server) writeGQLValue(w *jb, val gqlVal, sels []gqlSelection, ctx *gqlCtx) error {
	// Values are written directly (no comma): the caller wrote the key.
	w.afterKey = false
	if val.v == nil && val.typ != "scalar" {
		w.b = append(w.b, "null"...)
		return nil
	}
	switch val.typ {
	case "scalar":
		switch v := val.v.(type) {
		case nil:
			w.b = append(w.b, "null"...)
		case string:
			w.b = appendJSONStr(w.b, v)
		case bool:
			if v {
				w.b = append(w.b, "true"...)
			} else {
				w.b = append(w.b, "false"...)
			}
		case int:
			w.b = strconv.AppendInt(w.b, int64(v), 10)
		case float64:
			w.b = strconv.AppendFloat(w.b, v, 'f', -1, 64)
		default:
			w.b = append(w.b, "null"...)
		}
		return nil
	case "list":
		items := val.v.([]gqlVal)
		w.b = append(w.b, '[')
		for i, item := range items {
			if i > 0 {
				w.b = append(w.b, ',')
			}
			if err := s.writeGQLValue(w, item, sels, ctx); err != nil {
				return err
			}
		}
		w.b = append(w.b, ']')
		return nil
	default:
		// An object value: execute its selection set.
		sub := newJB(256)
		sub.obj()
		if err := s.execSelections(sub, val, sels, ctx); err != nil {
			return err
		}
		sub.end()
		w.b = append(w.b, sub.bytes()...)
		return nil
	}
}

// --- field resolution ---

// issueConn carries a paged issue list for the executor.
type issueConn struct {
	repo      *Repo
	issues    []*Issue // the page's issues, in order
	hasNext   bool
	endCursor string
}

type pullConn struct {
	pulls []*Pull
}

type pullFilesConn struct {
	files []pullFile
}

type typeDesc struct {
	name string
}

type fieldDesc struct {
	name string
}

type enumDesc struct {
	name string
}

func (s *server) resolveField(parent gqlVal, f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	switch parent.typ {
	case "Query":
		return s.resolveQueryField(f, ctx)
	case "Mutation":
		return s.resolveMutationField(f, ctx)
	case "User":
		login := parent.v.(string)
		if f.name == "login" {
			return scalarVal(login), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "User")
	case "Repository":
		return s.resolveRepoField(parent.v.(*Repo), f, ctx)
	case "Ref":
		if f.name == "name" {
			return scalarVal(parent.v.(string)), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "Ref")
	case "Issue":
		return s.resolveIssueField(parent.v.(*Issue), f, ctx)
	case "IssueConnection":
		c := parent.v.(*issueConn)
		switch f.name {
		case "nodes":
			items := make([]gqlVal, len(c.issues))
			for i, is := range c.issues {
				items[i] = gqlVal{typ: "Issue", v: is}
			}
			return gqlVal{typ: "list", v: items}, nil
		case "pageInfo":
			return gqlVal{typ: "PageInfo", v: c}, nil
		case "totalCount":
			return scalarVal(len(c.issues)), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "IssueConnection")
	case "PageInfo":
		c := parent.v.(*issueConn)
		switch f.name {
		case "hasNextPage":
			return scalarVal(c.hasNext), nil
		case "endCursor":
			if c.endCursor == "" {
				return scalarVal(nil), nil
			}
			return scalarVal(c.endCursor), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "PageInfo")
	case "PullRequest":
		return s.resolvePullField(parent.v.(*Pull), f, ctx)
	case "PullRequestConnection":
		c := parent.v.(*pullConn)
		if f.name == "nodes" {
			items := make([]gqlVal, len(c.pulls))
			for i, p := range c.pulls {
				items[i] = gqlVal{typ: "PullRequest", v: p}
			}
			return gqlVal{typ: "list", v: items}, nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "PullRequestConnection")
	case "PullRequestChangedFilesConnection":
		c := parent.v.(*pullFilesConn)
		if f.name == "nodes" {
			items := make([]gqlVal, len(c.files))
			for i := range c.files {
				items[i] = gqlVal{typ: "PullRequestChangedFile", v: c.files[i].path}
			}
			return gqlVal{typ: "list", v: items}, nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "PullRequestChangedFilesConnection")
	case "PullRequestChangedFile":
		if f.name == "path" {
			return scalarVal(parent.v.(string)), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, "PullRequestChangedFile")
	case "__Type":
		return s.resolveTypeField(parent.v.(*typeDesc), f, ctx)
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return scalarVal(parent.v.(string)), nil
		}
		if f.name == "description" {
			return scalarVal(nil), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, parent.typ)
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.name == "issue" {
			if parent.v == nil {
				return gqlVal{typ: "Issue"}, nil
			}
			return gqlVal{typ: "Issue", v: parent.v.(*Issue)}, nil
		}
		return gqlVal{}, unknownFieldErr(f.name, parent.typ)
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" {
			if parent.v == nil {
				return gqlVal{typ: "PullRequest"}, nil
			}
			return gqlVal{typ: "PullRequest", v: parent.v.(*Pull)}, nil
		}
		return gqlVal{}, unknownFieldErr(f.name, parent.typ)
	case "AddCommentPayload":
		if f.name == "commentEdge" {
			return gqlVal{typ: "IssueCommentEdge", v: parent.v}, nil
		}
		return gqlVal{}, unknownFieldErr(f.name, parent.typ)
	case "IssueCommentEdge":
		if f.name == "node" {
			return gqlVal{typ: "IssueComment", v: parent.v}, nil
		}
		return gqlVal{}, unknownFieldErr(f.name, parent.typ)
	case "IssueComment":
		c := parent.v.(*Comment)
		switch f.name {
		case "id":
			return scalarVal(commentGlobalID(c.ID)), nil
		case "body":
			return scalarVal(c.Body), nil
		}
		return gqlVal{}, unknownFieldErr(f.name, parent.typ)
	}
	return gqlVal{}, unknownFieldErr(f.name, parent.typ)
}

func (s *server) resolveQueryField(f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	switch f.name {
	case "viewer":
		return gqlVal{typ: "User", v: s.store.viewerLogin}, nil
	case "repository":
		owner := argStr(f, "owner", ctx.vars)
		name := argStr(f, "name", ctx.vars)
		repo := s.store.getRepo(owner, name)
		if repo == nil {
			return gqlVal{typ: "Repository"}, nil // null, no errors
		}
		return gqlVal{typ: "Repository", v: repo}, nil
	case "node":
		id := argStr(f, "id", ctx.vars)
		kind, repoDBID, number, ok := decodeGlobalID(id)
		if !ok {
			return gqlVal{typ: "Node"}, nil
		}
		switch kind {
		case "repo":
			repo := s.store.repoByDBID(repoDBID)
			if repo == nil {
				return gqlVal{typ: "Node"}, nil
			}
			return gqlVal{typ: "Repository", v: repo}, nil
		case "issue":
			repo := s.store.repoByDBID(repoDBID)
			if repo == nil {
				return gqlVal{typ: "Node"}, nil
			}
			is := repo.issueByNum[number]
			if is == nil {
				return gqlVal{typ: "Node"}, nil
			}
			return gqlVal{typ: "Issue", v: is}, nil
		case "pr":
			repo := s.store.repoByDBID(repoDBID)
			if repo == nil {
				return gqlVal{typ: "Node"}, nil
			}
			p := repo.pullByNum[number]
			if p == nil {
				return gqlVal{typ: "Node"}, nil
			}
			return gqlVal{typ: "PullRequest", v: p}, nil
		}
		return gqlVal{typ: "Node"}, nil
	case "__type":
		name := argStr(f, "name", ctx.vars)
		if _, ok := gqlTypeFields[name]; ok {
			return gqlVal{typ: "__Type", v: &typeDesc{name: name}}, nil
		}
		return gqlVal{typ: "__Type"}, nil
	}
	return gqlVal{}, unknownFieldErr(f.name, "Query")
}

// gqlTypeFields is the introspection schema: the fields each probed type
// lists. The suite requires the named fields to be present; extras are
// harmless.
var gqlTypeFields = map[string][]string{
	"Issue": {
		"id", "number", "title", "body", "state", "createdAt", "updatedAt",
		"author", "comments", "labels", "repository", "url",
	},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergeable",
		"headRefName", "baseRefName", "files", "createdAt", "updatedAt",
		"author", "comments",
	},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
		"viewerPermission", "hasIssuesEnabled", "issue", "issues",
		"pullRequests", "pullRequest", "description", "createdAt",
	},
	// SearchType is an enum: no fields, but __type must resolve it so
	// enumValues can be read.
	"SearchType": {},
}

var gqlSearchTypeValues = []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "MARKETPLACE"}

func (s *server) resolveTypeField(t *typeDesc, f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	switch f.name {
	case "name":
		return scalarVal(t.name), nil
	case "kind":
		if t.name == "SearchType" {
			return scalarVal("ENUM"), nil
		}
		return scalarVal("OBJECT"), nil
	case "description":
		return scalarVal(nil), nil
	case "fields":
		names := gqlTypeFields[t.name]
		items := make([]gqlVal, len(names))
		for i, n := range names {
			items[i] = gqlVal{typ: "__Field", v: n}
		}
		return gqlVal{typ: "list", v: items}, nil
	case "enumValues":
		if t.name != "SearchType" {
			return gqlVal{typ: "list", v: []gqlVal{}}, nil
		}
		items := make([]gqlVal, len(gqlSearchTypeValues))
		for i, n := range gqlSearchTypeValues {
			items[i] = gqlVal{typ: "__EnumValue", v: n}
		}
		return gqlVal{typ: "list", v: items}, nil
	}
	return gqlVal{}, unknownFieldErr(f.name, "__Type")
}

func (s *server) resolveRepoField(repo *Repo, f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	switch f.name {
	case "id":
		return scalarVal(repoGlobalID(repo.DatabaseID)), nil
	case "databaseId":
		return scalarVal(repo.DatabaseID), nil
	case "name":
		return scalarVal(repo.Name), nil
	case "nameWithOwner":
		return scalarVal(s.store.viewerLogin + "/" + repo.Name), nil
	case "description":
		return scalarVal(repo.Description), nil
	case "owner":
		return gqlVal{typ: "User", v: s.store.viewerLogin}, nil
	case "defaultBranchRef":
		if _, ok := repo.defaultTip(); !ok {
			return gqlVal{typ: "Ref"}, nil
		}
		return gqlVal{typ: "Ref", v: repo.DefaultBranch}, nil
	case "viewerPermission":
		return scalarVal("ADMIN"), nil
	case "hasIssuesEnabled":
		return scalarVal(true), nil
	case "issue":
		n, _ := argInt(f, "number", ctx.vars)
		is := repo.issueByNum[n]
		if is == nil {
			return gqlVal{typ: "Issue"}, nil
		}
		return gqlVal{typ: "Issue", v: is}, nil
	case "issues":
		return gqlVal{typ: "IssueConnection", v: s.issuesConnection(repo, f, ctx)}, nil
	case "pullRequests":
		first, _ := argInt(f, "first", ctx.vars)
		var pulls []*Pull
		for _, p := range repo.pulls {
			if p.State == "open" {
				pulls = append(pulls, p)
			}
		}
		if first > 0 && len(pulls) > first {
			pulls = pulls[:first]
		}
		return gqlVal{typ: "PullRequestConnection", v: &pullConn{pulls: pulls}}, nil
	case "pullRequest":
		n, _ := argInt(f, "number", ctx.vars)
		p := repo.pullByNum[n]
		if p == nil {
			return gqlVal{typ: "PullRequest"}, nil
		}
		return gqlVal{typ: "PullRequest", v: p}, nil
	}
	return gqlVal{}, unknownFieldErr(f.name, "Repository")
}

// issuesConnection applies states/orderBy/first/after to a repo's issues.
func (s *server) issuesConnection(repo *Repo, f *gqlField, ctx *gqlCtx) *issueConn {
	// states: [OPEN] / [CLOSED] / absent (all).
	var wantOpen, wantClosed bool
	if v, ok := f.args["states"]; ok {
		list := resolveValue(v, ctx.vars)
		if arr, ok := list.([]any); ok {
			for _, item := range arr {
				if str, ok := item.(string); ok {
					if str == "OPEN" {
						wantOpen = true
					}
					if str == "CLOSED" {
						wantClosed = true
					}
				}
			}
		}
	} else {
		wantOpen, wantClosed = true, true
	}
	// orderBy: {field, direction}; default CREATED_AT DESC.
	desc := true
	if v, ok := f.args["orderBy"]; ok {
		if m, ok := resolveValue(v, ctx.vars).(map[string]any); ok {
			if jstr(m, "direction") == "ASC" {
				desc = false
			}
		}
	}
	var ordered []*Issue
	if desc {
		for i := len(repo.issues) - 1; i >= 0; i-- {
			ordered = append(ordered, repo.issues[i])
		}
	} else {
		ordered = append(ordered, repo.issues...)
	}
	var filtered []*Issue
	for _, is := range ordered {
		if is.State == "open" && wantOpen {
			filtered = append(filtered, is)
		}
		if is.State == "closed" && wantClosed {
			filtered = append(filtered, is)
		}
	}
	// after: a cursor naming the last number of the previous page.
	after := argStr(f, "after", ctx.vars)
	if after != "" {
		if n, ok := decodeIssueCursor(after); ok {
			var rest []*Issue
			for _, is := range filtered {
				if desc && is.Number < n {
					rest = append(rest, is)
				}
				if !desc && is.Number > n {
					rest = append(rest, is)
				}
			}
			filtered = rest
		}
	}
	first, hasFirst := argInt(f, "first", ctx.vars)
	conn := &issueConn{repo: repo}
	if hasFirst && first >= 0 && len(filtered) > first {
		conn.issues = filtered[:first]
		conn.hasNext = true
	} else {
		conn.issues = filtered
	}
	if len(conn.issues) > 0 {
		conn.endCursor = encodeIssueCursor(conn.issues[len(conn.issues)-1].Number)
	}
	return conn
}

func encodeIssueCursor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func decodeIssueCursor(c string) (int, bool) {
	b, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *server) resolveIssueField(is *Issue, f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	switch f.name {
	case "id":
		// The global id needs the repo's database id; find it.
		for _, r := range s.store.repoOrder {
			if r.issueByNum[is.Number] == is {
				return scalarVal(issueGlobalID(r.DatabaseID, is.Number)), nil
			}
		}
		return scalarVal(issueGlobalID(0, is.Number)), nil
	case "number":
		return scalarVal(is.Number), nil
	case "title":
		return scalarVal(is.Title), nil
	case "body":
		return scalarVal(is.Body), nil
	case "state":
		if is.State == "open" {
			return scalarVal("OPEN"), nil
		}
		return scalarVal("CLOSED"), nil
	}
	return gqlVal{}, unknownFieldErr(f.name, "Issue")
}

func (s *server) resolvePullField(p *Pull, f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	switch f.name {
	case "id":
		for _, r := range s.store.repoOrder {
			if r.pullByNum[p.Number] == p {
				return scalarVal(pullGlobalID(r.DatabaseID, p.Number)), nil
			}
		}
		return scalarVal(pullGlobalID(0, p.Number)), nil
	case "number":
		return scalarVal(p.Number), nil
	case "title":
		return scalarVal(p.Title), nil
	case "body":
		return scalarVal(p.Body), nil
	case "state":
		if p.Merged {
			return scalarVal("MERGED"), nil
		}
		if p.State == "open" {
			return scalarVal("OPEN"), nil
		}
		return scalarVal("CLOSED"), nil
	case "merged":
		return scalarVal(p.Merged), nil
	case "headRefName":
		return scalarVal(p.HeadRef), nil
	case "baseRefName":
		return scalarVal(p.BaseRef), nil
	case "mergeable":
		repo := s.repoOfPull(p)
		if repo == nil {
			return scalarVal("UNKNOWN"), nil
		}
		if p.State != "open" {
			return scalarVal("UNKNOWN"), nil
		}
		if len(s.mergeConflict(repo, p)) > 0 {
			return scalarVal("CONFLICTING"), nil
		}
		return scalarVal("MERGEABLE"), nil
	case "files":
		repo := s.repoOfPull(p)
		if repo == nil {
			return gqlVal{typ: "PullRequestChangedFilesConnection", v: &pullFilesConn{}}, nil
		}
		files := s.pullFiles(repo, p)
		if first, ok := argInt(f, "first", ctx.vars); ok && first >= 0 && len(files) > first {
			files = files[:first]
		}
		return gqlVal{typ: "PullRequestChangedFilesConnection", v: &pullFilesConn{files: files}}, nil
	}
	return gqlVal{}, unknownFieldErr(f.name, "PullRequest")
}

func (s *server) repoOfPull(p *Pull) *Repo {
	for _, r := range s.store.repoOrder {
		if r.pullByNum[p.Number] == p {
			return r
		}
	}
	return nil
}

func (s *server) repoOfIssue(is *Issue) *Repo {
	for _, r := range s.store.repoOrder {
		if r.issueByNum[is.Number] == is {
			return r
		}
	}
	return nil
}

// --- mutations ---

func (s *server) resolveMutationField(f *gqlField, ctx *gqlCtx) (gqlVal, error) {
	input := argObj(f, "input", ctx.vars)
	switch f.name {
	case "createIssue":
		repoID := jstr(input, "repositoryId")
		kind, dbID, _, ok := decodeGlobalID(repoID)
		if !ok || kind != "repo" {
			return gqlVal{}, gqlError("createIssue: unknown repository id")
		}
		repo := s.store.repoByDBID(dbID)
		if repo == nil {
			return gqlVal{}, gqlError("createIssue: unknown repository id")
		}
		issue, errMsg := s.store.newIssue(repo, jstr(input, "title"), jstr(input, "body"), nil)
		if errMsg != "" {
			return gqlVal{}, gqlError(errMsg)
		}
		return gqlVal{typ: "CreateIssuePayload", v: issue}, nil
	case "closeIssue":
		issueID := jstr(input, "issueId")
		kind, dbID, number, ok := decodeGlobalID(issueID)
		if !ok || kind != "issue" {
			return gqlVal{}, gqlError("closeIssue: unknown issue id")
		}
		repo := s.store.repoByDBID(dbID)
		if repo == nil {
			return gqlVal{}, gqlError("closeIssue: unknown issue id")
		}
		issue := repo.issueByNum[number]
		if issue == nil {
			return gqlVal{}, gqlError("closeIssue: unknown issue id")
		}
		issue.State = "closed"
		issue.UpdatedAt = s.store.nowISO
		issue.jsonCache = nil
		return gqlVal{typ: "CloseIssuePayload", v: issue}, nil
	case "createPullRequest":
		repoID := jstr(input, "repositoryId")
		kind, dbID, _, ok := decodeGlobalID(repoID)
		if !ok || kind != "repo" {
			return gqlVal{}, gqlError("createPullRequest: unknown repository id")
		}
		repo := s.store.repoByDBID(dbID)
		if repo == nil {
			return gqlVal{}, gqlError("createPullRequest: unknown repository id")
		}
		pull, errMsg := s.store.newPull(repo, jstr(input, "title"), jstr(input, "body"), jstr(input, "headRefName"), jstr(input, "baseRefName"))
		if errMsg != "" {
			return gqlVal{}, gqlError(errMsg)
		}
		return gqlVal{typ: "CreatePullRequestPayload", v: pull}, nil
	case "mergePullRequest":
		prID := jstr(input, "pullRequestId")
		kind, dbID, number, ok := decodeGlobalID(prID)
		if !ok || kind != "pr" {
			return gqlVal{}, gqlError("mergePullRequest: unknown pull request id")
		}
		repo := s.store.repoByDBID(dbID)
		if repo == nil {
			return gqlVal{}, gqlError("mergePullRequest: unknown pull request id")
		}
		pull := repo.pullByNum[number]
		if pull == nil {
			return gqlVal{}, gqlError("mergePullRequest: unknown pull request id")
		}
		if _, status, msg := s.doMerge(repo, pull); status != 200 {
			return gqlVal{}, gqlError(msg)
		}
		return gqlVal{typ: "MergePullRequestPayload", v: pull}, nil
	case "addComment":
		subjectID := jstr(input, "subjectId")
		kind, dbID, number, ok := decodeGlobalID(subjectID)
		if !ok || kind != "issue" {
			return gqlVal{}, gqlError("addComment: unknown subject id")
		}
		repo := s.store.repoByDBID(dbID)
		if repo == nil {
			return gqlVal{}, gqlError("addComment: unknown subject id")
		}
		issue := repo.issueByNum[number]
		if issue == nil {
			return gqlVal{}, gqlError("addComment: unknown subject id")
		}
		c := &Comment{
			ID:        s.store.nextCommentID,
			IssueNum:  issue.Number,
			Body:      jstr(input, "body"),
			CreatedAt: s.store.nowISO,
		}
		s.store.nextCommentID++
		issue.Comments = append(issue.Comments, c)
		return gqlVal{typ: "AddCommentPayload", v: c}, nil
	}
	return gqlVal{}, unknownFieldErr(f.name, "Mutation")
}
