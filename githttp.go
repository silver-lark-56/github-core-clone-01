// githttp.go: git smart HTTP — protocol v2 (ls-refs, fetch) with a v0
// fallback, and receive-pack v0 with report-status and side-band-64k.
// All pkt-line framing lives here; object math lives in gitobj.go.
package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const (
	agentName   = "forge/1.0"
	pktLineMax  = 65520
	sidebandMax = pktLineMax - 4 - 1 // payload bytes per sideband packet
)

// pktLine frames one payload as a pkt-line (4 hex length digits +
// payload). A nil payload is a flush packet ("0000").
func pktLine(payload []byte) []byte {
	if payload == nil {
		return []byte("0000")
	}
	n := len(payload) + 4
	var b [4]byte
	b[0] = hexDigit(byte(n >> 12))
	b[1] = hexDigit(byte(n >> 8))
	b[2] = hexDigit(byte(n >> 4))
	b[3] = hexDigit(byte(n))
	out := make([]byte, 0, n)
	out = append(out, b[:]...)
	out = append(out, payload...)
	return out
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

const pktFlush = "0000"
const pktDelim = "0001"

// pktKind classifies one pkt-line's payload.
type pktKind int

const (
	pktData pktKind = iota
	pktFlushP
	pktDelimP
)

// parsePktLines decodes a pkt-line stream into (kind, payload) pairs.
// It stops at the first malformed line and reports how many input bytes
// were consumed. When stopAtFlush is set, parsing ends right after the
// first flush packet — a receive-pack command list is followed by raw
// pack bytes that are not pkt-lines and must not be parsed as such.
func parsePktLines(b []byte, stopAtFlush bool) ([]struct {
	kind    pktKind
	payload []byte
}, int, bool) {
	var out []struct {
		kind    pktKind
		payload []byte
	}
	pos := 0
	for pos+4 <= len(b) {
		if bytes.Equal(b[pos:pos+4], []byte("0000")) {
			out = append(out, struct {
				kind    pktKind
				payload []byte
			}{pktFlushP, nil})
			pos += 4
			if stopAtFlush {
				return out, pos, true
			}
			continue
		}
		if bytes.Equal(b[pos:pos+4], []byte("0001")) {
			out = append(out, struct {
				kind    pktKind
				payload []byte
			}{pktDelimP, nil})
			pos += 4
			continue
		}
		n := 0
		for i := 0; i < 4; i++ {
			v, ok := hexVal(b[pos+i])
			if !ok {
				return out, pos, false
			}
			n = n<<4 | int(v)
		}
		if n < 4 || pos+n > len(b) {
			return out, pos, false
		}
		out = append(out, struct {
			kind    pktKind
			payload []byte
		}{pktData, b[pos+4 : pos+n]})
		pos += n
	}
	return out, pos, true
}

// gitPath splits "/{owner}/{repo}.git/<service>" into its parts.
func gitPath(p string) (owner, repo, service string, ok bool) {
	// p starts with "/".
	rest := strings.TrimPrefix(p, "/")
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", "", false
	}
	owner = rest[:i]
	rest = rest[i+1:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return "", "", "", false
	}
	repoGit := rest[:j]
	service = rest[j+1:]
	if !strings.HasSuffix(repoGit, ".git") {
		return "", "", "", false
	}
	repo = strings.TrimSuffix(repoGit, ".git")
	return owner, repo, service, true
}

// gitResp builds a git response with the right content type.
func (s *server) gitResp(status int, contentType string, body []byte) *response {
	return &response{
		status: status,
		headers: [][2]string{
			{"Content-Type", contentType},
			{"X-GitHub-Request-Id", s.store.nextRequestID()},
		},
		body: body,
	}
}

func (s *server) handleGit(req *request) *response {
	if resp := s.requireAuth(req); resp != nil {
		return resp
	}
	owner, repoName, service, ok := gitPath(req.path)
	if !ok {
		return s.jsonErr(404, "Not Found")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.store.getRepo(owner, repoName)
	if repo == nil {
		return s.jsonErr(404, "Repository not found")
	}
	switch service {
	case "info/refs":
		if req.method != "GET" {
			return s.jsonErr(405, "Method not allowed")
		}
		svc, _ := req.queryValue("service")
		return s.gitInfoRefs(repo, svc, req)
	case "git-upload-pack":
		if req.method != "POST" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.gitUploadPack(repo, req)
	case "git-receive-pack":
		if req.method != "POST" {
			return s.jsonErr(405, "Method not allowed")
		}
		return s.gitReceivePack(repo, req)
	}
	return s.jsonErr(404, "Not Found")
}

// --- advertisements ---

// v2Advertisement is constant: capabilities only, no refs.
var v2Advertisement = func() []byte {
	var b bytes.Buffer
	b.Write(pktStr("version 2\n"))
	b.Write(pktStr("fetch=shallow\n"))
	b.Write(pktStr("ls-refs=unborn\n"))
	b.Write(pktStr("object-format=sha1\n"))
	b.Write(pktStr("agent=" + agentName + "\n"))
	b.WriteString(pktFlush)
	return b.Bytes()
}()

func (s *server) gitInfoRefs(repo *Repo, service string, req *request) *response {
	switch service {
	case "git-upload-pack":
		if req.header("git-protocol") == "version=2" {
			return s.gitResp(200, "application/x-git-upload-pack-advertisement", v2Advertisement)
		}
		return s.gitResp(200, "application/x-git-upload-pack-advertisement", s.v0Advertisement(repo, true))
	case "git-receive-pack":
		return s.gitResp(200, "application/x-git-receive-pack-advertisement", s.v0Advertisement(repo, false))
	}
	return s.jsonErr(404, "Not Found")
}

// v0Advertisement builds (and caches) a v0 ref advertisement. Upload
// and receive differ only in the service name and capabilities.
func (s *server) v0Advertisement(repo *Repo, upload bool) []byte {
	if upload {
		if repo.advV0Up != nil && repo.advV0UpV == repo.version {
			return repo.advV0Up
		}
	} else {
		if repo.advV0Rec != nil && repo.advV0RecV == repo.version {
			return repo.advV0Rec
		}
	}
	service := "git-upload-pack"
	caps := "side-band-64k thin-pack ofs-delta multi_ack_detailed shallow symref=HEAD:refs/heads/" + repo.DefaultBranch + " agent=" + agentName
	if !upload {
		service = "git-receive-pack"
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + agentName
	}
	var b bytes.Buffer
	b.Write(pktStr("# service=" + service + "\n"))
	b.WriteString(pktFlush)
	names := repo.sortedRefNames()
	headOid, headOk := repo.defaultTip()
	if len(names) == 0 {
		// Empty repository: the capabilities line.
		b.Write(pktStr(ZeroOid.hex() + " capabilities^{}\x00" + caps + "\n"))
	} else {
		first := true
		// HEAD first, carrying the capabilities.
		if headOk {
			line := headOid.hex() + " HEAD\x00" + caps + "\n"
			b.Write(pktStr(line))
			first = false
		}
		for _, name := range names {
			oid := repo.refs[name]
			line := oid.hex() + " " + name
			if first {
				line += "\x00" + caps
				first = false
			}
			// Peeled tag lines, as git emits them.
			b.Write(pktStr(line + "\n"))
			if strings.HasPrefix(name, "refs/tags/") {
				if o := repo.objects[oid]; o != nil && o.typ == objTag {
					if peeled := repo.peel(oid); peeled != oid {
						b.Write(pktStr(peeled.hex() + " " + name + "^{}\n"))
					}
				}
			}
		}
	}
	b.WriteString(pktFlush)
	out := b.Bytes()
	if upload {
		repo.advV0Up = out
		repo.advV0UpV = repo.version
	} else {
		repo.advV0Rec = out
		repo.advV0RecV = repo.version
	}
	return out
}

// --- upload-pack ---

func (s *server) gitUploadPack(repo *Repo, req *request) *response {
	packets, _, ok := parsePktLines(req.body, false)
	if !ok || len(packets) == 0 {
		// Malformed request: refuse it, stay healthy.
		return s.gitResp(400, "application/x-git-upload-pack-result", nil)
	}
	// v2 if the first line is a command, v0 if it is a want line.
	first := packets[0]
	if first.kind != pktData {
		return s.gitResp(400, "application/x-git-upload-pack-result", nil)
	}
	line := string(first.payload)
	if strings.HasPrefix(line, "command=") {
		return s.gitUploadPackV2(repo, packets)
	}
	if strings.HasPrefix(line, "want ") {
		return s.gitUploadPackV0(repo, packets)
	}
	return s.gitResp(400, "application/x-git-upload-pack-result", nil)
}

// ls-refs / fetch argument parsing shared by the v2 commands.
type v2args struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
	wants    []Oid
	haves    []Oid
	shallow  []Oid
	depth    int
	done     bool
}

func parseV2Args(packets []struct {
	kind    pktKind
	payload []byte
}) v2args {
	var a v2args
	inArgs := false
	for _, p := range packets {
		if p.kind == pktDelimP {
			inArgs = true
			continue
		}
		if p.kind != pktData || !inArgs {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			a.peel = true
		case line == "symrefs":
			a.symrefs = true
		case line == "unborn":
			a.unborn = true
		case line == "done":
			a.done = true
		case strings.HasPrefix(line, "ref-prefix "):
			a.prefixes = append(a.prefixes, line[len("ref-prefix "):])
		case strings.HasPrefix(line, "want "):
			if o, ok := oidFromHex(strings.TrimSpace(line[5:])); ok {
				a.wants = append(a.wants, o)
			}
		case strings.HasPrefix(line, "have "):
			if o, ok := oidFromHex(strings.TrimSpace(line[5:])); ok {
				a.haves = append(a.haves, o)
			}
		case strings.HasPrefix(line, "shallow "):
			if o, ok := oidFromHex(strings.TrimSpace(line[8:])); ok {
				a.shallow = append(a.shallow, o)
			}
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				a.depth = n
			}
		}
	}
	return a
}

func (s *server) gitUploadPackV2(repo *Repo, packets []struct {
	kind    pktKind
	payload []byte
}) *response {
	cmd := strings.TrimSuffix(string(packets[0].payload), "\n")
	cmd = strings.TrimPrefix(cmd, "command=")
	args := parseV2Args(packets)
	switch cmd {
	case "ls-refs":
		return s.gitResp(200, "application/x-git-upload-pack-result", s.lsRefs(repo, args))
	case "fetch":
		return s.gitResp(200, "application/x-git-upload-pack-result", s.fetchV2(repo, args))
	}
	// Unknown v2 command: a JSON-RPC-style refusal is not git's way;
	// answer an empty result so the client sees no refs rather than a
	// hung connection.
	return s.gitResp(200, "application/x-git-upload-pack-result", []byte(pktFlush))
}

// lsRefs answers a v2 ls-refs command, cached per repo version + args.
func (s *server) lsRefs(repo *Repo, args v2args) []byte {
	key := "p=" + strconv.FormatBool(args.peel) + ",s=" + strconv.FormatBool(args.symrefs) +
		",u=" + strconv.FormatBool(args.unborn) + ",x=" + strings.Join(args.prefixes, "|")
	if repo.lsCacheV == repo.version {
		if cached, ok := repo.lsCache[key]; ok {
			return cached
		}
	} else {
		repo.lsCache = make(map[string][]byte)
		repo.lsCacheV = repo.version
	}
	var b bytes.Buffer
	keep := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, p := range args.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	headOid, headOk := repo.defaultTip()
	headTarget := "refs/heads/" + repo.DefaultBranch
	if keep("HEAD") {
		if headOk {
			line := headOid.hex() + " HEAD"
			if args.symrefs {
				line += " symref-target:" + headTarget
			}
			b.Write(pktStr(line + "\n"))
		} else if args.unborn {
			b.Write(pktStr("unborn HEAD symref-target:" + headTarget + "\n"))
		}
	}
	for _, name := range repo.sortedRefNames() {
		if !keep(name) {
			continue
		}
		oid := repo.refs[name]
		line := oid.hex() + " " + name
		if args.peel && strings.HasPrefix(name, "refs/tags/") {
			if o := repo.objects[oid]; o != nil && o.typ == objTag {
				if peeled := repo.peel(oid); peeled != oid {
					line += " peeled:" + peeled.hex()
				}
			}
		}
		b.Write(pktStr(line + "\n"))
	}
	b.WriteString(pktFlush)
	out := b.Bytes()
	repo.lsCache[key] = out
	return out
}

// fetchV2 answers a v2 fetch command: acknowledgments, shallow-info
// (when non-empty) and packfile (when objects are due), each section
// ended by a flush. The framed bytes are memoized on the entry.
func (s *server) fetchV2(repo *Repo, args v2args) []byte {
	entry := s.fetchEntry(repo, args.wants, args.haves, args.shallow, args.depth)
	ready := args.done && len(entry.acks) > 0
	if ready {
		if entry.framedV2Ready != nil {
			return entry.framedV2Ready
		}
	} else if entry.framedV2 != nil {
		return entry.framedV2
	}
	var b bytes.Buffer
	b.Write(pktStr("acknowledgments\n"))
	if len(entry.acks) == 0 {
		b.Write(pktStr("NAK\n"))
	} else {
		for _, ack := range entry.acks {
			b.Write(pktStr("ACK " + ack + "\n"))
		}
		if ready {
			b.Write(pktStr("ready\n"))
		}
	}
	b.WriteString(pktFlush)
	if len(entry.shallow) > 0 || len(entry.unshallow) > 0 {
		b.Write(pktStr("shallow-info\n"))
		for _, oid := range entry.shallow {
			b.Write(pktStr("shallow " + oid + "\n"))
		}
		for _, oid := range entry.unshallow {
			b.Write(pktStr("unshallow " + oid + "\n"))
		}
		b.WriteString(pktFlush)
	}
	if len(entry.pack) > 0 {
		b.Write(pktStr("packfile\n"))
		writeSideband(&b, entry.pack)
		b.WriteString(pktFlush)
	}
	out := b.Bytes()
	if ready {
		entry.framedV2Ready = out
	} else {
		entry.framedV2 = out
	}
	return out
}

// fetchEntry computes (or reuses a cached) fetch result. The key
// includes the object count: objects are grow-only per repo, so the
// count is a monotonic version — a want oid that was unknown at an
// earlier fetch (empty pack cached) can never hit that stale entry
// after the object arrives.
func (s *server) fetchEntry(repo *Repo, wants, haves, shallow []Oid, depth int) *fetchCacheEntry {
	key := fetchKey(repo, wants, haves, shallow, depth)
	if e, ok := repo.fetchCache[key]; ok {
		return e
	}
	closure := repo.fetchClosure(wants, haves, shallow, depth)
	acks := repo.commonHaves(wants, haves)
	var pack []byte
	if len(closure.objects) > 0 {
		ids := make([]Oid, 0, len(closure.objects))
		for oid := range closure.objects {
			ids = append(ids, oid)
		}
		pack = writePack(repo.objects, ids)
	}
	e := &fetchCacheEntry{pack: pack, shallow: closure.shallow, unshallow: closure.unshallow, acks: acks}
	if len(repo.fetchCache) > 256 {
		repo.fetchCache = make(map[string]*fetchCacheEntry)
	}
	repo.fetchCache[key] = e
	return e
}

func fetchKey(repo *Repo, wants, haves, shallow []Oid, depth int) string {
	var b bytes.Buffer
	b.WriteString("n:")
	b.WriteString(strconv.Itoa(len(repo.objects)))
	b.WriteString("|w:")
	for _, o := range sortedOids(wants) {
		b.WriteString(o.hex())
		b.WriteByte(',')
	}
	b.WriteString("|h:")
	for _, o := range sortedOids(haves) {
		b.WriteString(o.hex())
		b.WriteByte(',')
	}
	b.WriteString("|s:")
	for _, o := range sortedOids(shallow) {
		b.WriteString(o.hex())
		b.WriteByte(',')
	}
	b.WriteString("|d:")
	b.WriteString(strconv.Itoa(depth))
	return b.String()
}

func sortedOids(in []Oid) []Oid {
	out := make([]Oid, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	return out
}

// writeSideband splits pack bytes into band-1 sideband packets.
func writeSideband(b *bytes.Buffer, pack []byte) {
	for len(pack) > 0 {
		n := len(pack)
		if n > sidebandMax {
			n = sidebandMax
		}
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, pack[:n]...)
		b.Write(pktLine(payload))
		pack = pack[n:]
	}
}

// gitUploadPackV0 answers a v0 fetch: shallow/unshallow lines, NAK/ACK,
// then the sideband pack.
func (s *server) gitUploadPackV0(repo *Repo, packets []struct {
	kind    pktKind
	payload []byte
}) *response {
	var wants, haves, shallow []Oid
	depth := 0
	inHaves := false
	for _, p := range packets {
		if p.kind == pktFlushP {
			inHaves = true
			continue
		}
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := line[5:]
			if i := strings.IndexByte(rest, ' '); i >= 0 {
				rest = rest[:i] // drop capabilities on the first want
			}
			if o, ok := oidFromHex(rest); ok {
				wants = append(wants, o)
			}
		case strings.HasPrefix(line, "have ") && inHaves:
			if o, ok := oidFromHex(strings.TrimSpace(line[5:])); ok {
				haves = append(haves, o)
			}
		case strings.HasPrefix(line, "shallow "):
			if o, ok := oidFromHex(strings.TrimSpace(line[8:])); ok {
				shallow = append(shallow, o)
			}
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				depth = n
			}
		}
	}
	entry := s.fetchEntry(repo, wants, haves, shallow, depth)
	if entry.framedV0 != nil {
		return s.gitResp(200, "application/x-git-upload-pack-result", entry.framedV0)
	}
	var b bytes.Buffer
	for _, oid := range entry.shallow {
		b.Write(pktStr("shallow " + oid + "\n"))
	}
	for _, oid := range entry.unshallow {
		b.Write(pktStr("unshallow " + oid + "\n"))
	}
	if len(entry.acks) == 0 {
		b.Write(pktStr("NAK\n"))
	} else {
		for _, ack := range entry.acks {
			b.Write(pktStr("ACK " + ack + "\n"))
		}
	}
	if len(entry.pack) > 0 {
		writeSideband(&b, entry.pack)
	}
	b.WriteString(pktFlush)
	entry.framedV0 = b.Bytes()
	return s.gitResp(200, "application/x-git-upload-pack-result", entry.framedV0)
}

// --- receive-pack ---

type pushCommand struct {
	oldOid Oid
	newOid Oid
	ref    string
}

func (s *server) gitReceivePack(repo *Repo, req *request) *response {
	packets, consumed, ok := parsePktLines(req.body, true)
	if !ok || len(packets) == 0 {
		return s.gitResp(400, "application/x-git-receive-pack-result", nil)
	}
	var commands []pushCommand
	sideband := false
	sawFlush := false
	for _, p := range packets {
		if p.kind == pktFlushP {
			sawFlush = true
			break
		}
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		// Capabilities ride on the first command after a NUL.
		if i := strings.IndexByte(line, 0); i >= 0 {
			caps := line[i+1:]
			line = line[:i]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return s.gitResp(400, "application/x-git-receive-pack-result", nil)
		}
		oldOid, ok1 := oidFromHex(parts[0])
		newOid, ok2 := oidFromHex(parts[1])
		if !ok1 || !ok2 {
			return s.gitResp(400, "application/x-git-receive-pack-result", nil)
		}
		commands = append(commands, pushCommand{oldOid: oldOid, newOid: newOid, ref: parts[2]})
	}
	if !sawFlush || len(commands) == 0 {
		return s.gitResp(400, "application/x-git-receive-pack-result", nil)
	}
	// Everything after the command flush is the pack (possibly empty:
	// a delete-only push carries no pack at all).
	packBytes := req.body[consumed:]
	unpackOk := true
	if len(packBytes) > 0 {
		objects, err := repo.readPack(packBytes)
		if err != nil {
			unpackOk = false
		} else {
			for oid, o := range objects {
				if _, exists := repo.objects[oid]; !exists {
					repo.objects[oid] = o
				}
			}
		}
	}
	// The report, pkt-framed.
	var report bytes.Buffer
	if !unpackOk {
		report.Write(pktStr("unpack error\n"))
	} else {
		// Every non-zero new oid must exist after the unpack.
		missing := false
		for _, c := range commands {
			if !c.newOid.isZero() {
				if _, ok := repo.objects[c.newOid]; !ok {
					missing = true
				}
			}
		}
		if missing {
			report.Write(pktStr("unpack error\n"))
		} else {
			report.Write(pktStr("unpack ok\n"))
			for _, c := range commands {
				if s.applyPush(repo, c) {
					report.Write(pktStr("ok " + c.ref + "\n"))
				} else {
					report.Write(pktStr("ng " + c.ref + " non-fast-forward\n"))
				}
			}
		}
	}
	report.WriteString(pktFlush)
	var b bytes.Buffer
	if sideband {
		// The pkt-framed report rides inside band-1 packets.
		data := report.Bytes()
		for len(data) > 0 {
			n := len(data)
			if n > sidebandMax {
				n = sidebandMax
			}
			payload := make([]byte, 0, n+1)
			payload = append(payload, 1)
			payload = append(payload, data[:n]...)
			b.Write(pktLine(payload))
			data = data[n:]
		}
		b.WriteString(pktFlush)
	} else {
		b.Write(report.Bytes())
	}
	return s.gitResp(200, "application/x-git-receive-pack-result", b.Bytes())
}

// applyPush validates and applies one ref update: create (old zero),
// delete (new zero) and fast-forward only. Anything else is refused
// per-ref with reason "non-fast-forward".
func (s *server) applyPush(repo *Repo, c pushCommand) bool {
	cur, exists := repo.refs[c.ref]
	if !exists {
		if !c.oldOid.isZero() {
			return false
		}
		if c.newOid.isZero() {
			return false // deleting a ref that does not exist
		}
		repo.setRef(c.ref, c.newOid)
		return true
	}
	if cur != c.oldOid {
		return false
	}
	if c.newOid.isZero() {
		repo.deleteRef(c.ref)
		return true
	}
	if c.newOid == cur {
		return true // already up to date
	}
	if !repo.isAncestorOf(cur, c.newOid) {
		return false
	}
	repo.setRef(c.ref, c.newOid)
	return true
}
