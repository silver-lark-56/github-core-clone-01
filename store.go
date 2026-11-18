// store.go: the in-memory store. One process, one owner namespace, no
// persistence: every trial starts empty and the suite builds what it needs
// through the API. All access is serialized by Store.mu; handlers do their
// I/O outside the lock and hold it only while touching state.
//
// Determinism: every timestamp comes from the fixed trial now (never the
// wall clock); every id is a sequential counter; map iteration order never
// reaches a response (lists are sorted or insertion-ordered).
package main

import (
	"encoding/base64"
	"strconv"
	"sync"
	"time"
)

// Oid is a git object id (SHA-1, 20 bytes).
type Oid [20]byte

// ZeroOid is the all-zero object id.
var ZeroOid Oid

func oidFromHex(s string) (Oid, bool) {
	var o Oid
	if len(s) != 40 {
		return o, false
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := hexVal(s[i*2])
		lo, ok2 := hexVal(s[i*2+1])
		if !ok1 || !ok2 {
			return o, false
		}
		o[i] = hi<<4 | lo
	}
	return o, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (o Oid) hex() string {
	const digits = "0123456789abcdef"
	var b [40]byte
	for i, v := range o {
		b[i*2] = digits[v>>4]
		b[i*2+1] = digits[v&0xf]
	}
	return string(b[:])
}

func (o Oid) isZero() bool { return o == ZeroOid }

// Git object types (pack type numbers).
const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

func objTypeName(t byte) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return ""
}

func objTypeNum(name string) byte {
	switch name {
	case "commit":
		return objCommit
	case "tree":
		return objTree
	case "blob":
		return objBlob
	case "tag":
		return objTag
	}
	return 0
}

// Obj is one git object: its type and raw (uncompressed) bytes.
type Obj struct {
	typ  byte
	data []byte
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Comment is an issue comment.
type Comment struct {
	ID        int // global sequential id (>= 1)
	IssueNum  int
	Body      string
	CreatedAt string // ISO 8601, fixed trial now
}

// Issue is a repository issue.
type Issue struct {
	ID        int // global sequential REST id
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []*Label
	Comments  []*Comment
	CreatedAt string
	UpdatedAt string
	// jsonCache holds the serialized REST representation, invalidated on
	// any mutation of this issue. The read benchmark GETs one issue
	// 2000 times on a warm connection; the cache makes that a memcpy.
	jsonCache []byte
}

// Pull is a pull request. HeadSHA/BaseSHA are snapshots taken at creation.
type Pull struct {
	ID            int // global sequential REST id
	Number        int
	Title         string
	Body          string
	State         string // "open" | "closed"
	Merged        bool
	MergedAt      string // "" until merged
	MergeCommit   string // "" until merged (hex sha)
	HeadRef       string
	BaseRef       string
	HeadSHA       string
	BaseSHA       string
	CreatedAt     string
	UpdatedAt     string
}

// fetchCacheEntry is one memoized fetch computation: the pack bytes plus
// the shallow/unshallow/ack lines. Objects are append-only, so a cached
// pack stays valid after later pushes (the fetch key also carries the
// object count, so a want that was unknown at an earlier fetch can
// never hit a stale empty entry). The fully framed v2/v0 responses are
// memoized too (framedV2Ready is the variant with the "ready" line,
// used when the client sent done and there are ACKs): a cache-hit
// clone is then a single byte copy.
type fetchCacheEntry struct {
	pack          []byte
	shallow       []string
	unshallow     []string
	acks          []string
	framedV2      []byte // nil until framed (a framed response is never empty)
	framedV2Ready []byte
	framedV0      []byte
}

// Repo is one repository: its git object store and refs plus its
// issues, pull requests and labels.
type Repo struct {
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	DatabaseID    int
	CreatedAt     string

	objects map[Oid]*Obj
	refs    map[string]Oid // full refname -> tip

	labels     map[string]*Label
	labelOrder []*Label
	issues     []*Issue
	issueByNum map[int]*Issue
	pulls      []*Pull
	pullByNum  map[int]*Pull
	nextNum    int // shared issue/PR number counter (GitHub shares it)

	// version bumps on any ref change; git advertisement caches key on it.
	version  uint64
	advV0Up  []byte // cached v0 upload-pack advertisement
	advV0UpV uint64
	advV0Rec []byte // cached v0 receive-pack advertisement
	advV0RecV uint64
	lsCache  map[string][]byte // ls-refs responses by args key
	lsCacheV uint64

	fetchCache map[string]*fetchCacheEntry
}

// Store is the whole forge state.
type Store struct {
	mu sync.Mutex

	// viewerLogin is the login the bearer token names. The server never
	// learns it from the environment: it is bound lazily from the first
	// owner-named request (the git checks run first and always use the
	// viewer login), and any other owner then reads 404, never 403.
	viewerLogin string

	repos     map[string]*Repo // by name (single owner namespace)
	repoOrder []*Repo

	nextRepoID    int
	nextCommentID int
	nextRestID    int // global REST ids for issues and pull requests

	// trialNowMs is MAKEFUN_TRIAL_NOW (RFC 3339 or epoch ms), parsed once
	// at startup; the only "now" any timestamp may carry.
	trialNowMs int64
	nowISO     string
	nowUnixSec int64

	// reqSeq numbers X-GitHub-Request-Id headers.
	reqSeq uint64
}

func NewStore(trialNowMs int64) *Store {
	s := &Store{
		repos:         make(map[string]*Repo),
		nextRepoID:    1,
		nextCommentID: 1,
		nextRestID:    1,
		trialNowMs:    trialNowMs,
	}
	t := time.UnixMilli(trialNowMs).UTC()
	s.nowISO = t.Format("2006-01-02T15:04:05Z")
	s.nowUnixSec = t.Unix()
	return s
}

// bindViewer records the token owner's login from the first owner-named
// request and reports whether owner is that login. Call with mu held.
func (s *Store) bindViewer(owner string) bool {
	if s.viewerLogin == "" {
		s.viewerLogin = owner
		return true
	}
	return s.viewerLogin == owner
}

// getRepo resolves owner/name: false when the owner is foreign or the
// repository does not exist (both are 404, never 403). Call with mu held.
func (s *Store) getRepo(owner, name string) *Repo {
	if !s.bindViewer(owner) {
		return nil
	}
	return s.repos[name]
}

// createRepo makes a repository; ok is false when the name is taken.
// Call with mu held.
func (s *Store) createRepo(name string, private bool, description string) (*Repo, bool) {
	if _, exists := s.repos[name]; exists {
		return nil, false
	}
	r := &Repo{
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		DatabaseID:    s.nextRepoID,
		CreatedAt:     s.nowISO,
		objects:       make(map[Oid]*Obj),
		refs:          make(map[string]Oid),
		labels:        make(map[string]*Label),
		issueByNum:    make(map[int]*Issue),
		pullByNum:     make(map[int]*Pull),
		nextNum:       1,
		lsCache:       make(map[string][]byte),
		fetchCache:    make(map[string]*fetchCacheEntry),
	}
	s.nextRepoID++
	s.repos[name] = r
	s.repoOrder = append(s.repoOrder, r)
	return r, true
}

// deleteRepo removes a repository and all its state. Call with mu held.
func (s *Store) deleteRepo(name string) bool {
	r, ok := s.repos[name]
	if !ok {
		return false
	}
	delete(s.repos, name)
	for i, cand := range s.repoOrder {
		if cand == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// nextRequestID mints the next X-GitHub-Request-Id value.
func (s *Store) nextRequestID() string {
	s.reqSeq++
	return "forge-" + strconv.FormatUint(s.reqSeq, 10)
}

// --- global (Relay) ids: opaque, stable, resolvable across surfaces ---

var b64url = base64.RawURLEncoding

func repoGlobalID(dbID int) string {
	return b64url.EncodeToString([]byte("repo:" + strconv.Itoa(dbID)))
}

func issueGlobalID(repoDBID, number int) string {
	return b64url.EncodeToString([]byte("issue:" + strconv.Itoa(repoDBID) + ":" + strconv.Itoa(number)))
}

func pullGlobalID(repoDBID, number int) string {
	return b64url.EncodeToString([]byte("pr:" + strconv.Itoa(repoDBID) + ":" + strconv.Itoa(number)))
}

func commentGlobalID(id int) string {
	return b64url.EncodeToString([]byte("comment:" + strconv.Itoa(id)))
}

// decodeGlobalID parses a global id into (kind, repoDBID, number, ok).
// kind is "repo", "issue", "pr" or "comment"; for "comment", number is
// the comment's global id and repoDBID is 0.
func decodeGlobalID(id string) (kind string, repoDBID, number int, ok bool) {
	b, err := b64url.DecodeString(id)
	if err != nil {
		return "", 0, 0, false
	}
	s := string(b)
	// Split "kind:a[:b]" without allocating more than needed.
	i := 0
	for i < len(s) && s[i] != ':' {
		i++
	}
	if i == len(s) {
		return "", 0, 0, false
	}
	kind = s[:i]
	rest := s[i+1:]
	switch kind {
	case "repo":
		n, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, 0, false
		}
		return kind, n, 0, true
	case "comment":
		n, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, 0, false
		}
		return kind, 0, n, true
	case "issue", "pr":
		j := 0
		for j < len(rest) && rest[j] != ':' {
			j++
		}
		if j == len(rest) {
			return "", 0, 0, false
		}
		a, err1 := strconv.Atoi(rest[:j])
		b2, err2 := strconv.Atoi(rest[j+1:])
		if err1 != nil || err2 != nil {
			return "", 0, 0, false
		}
		return kind, a, b2, true
	}
	return "", 0, 0, false
}

// repoByDBID finds a repository by its numeric database id. Call with mu held.
func (s *Store) repoByDBID(dbID int) *Repo {
	for _, r := range s.repoOrder {
		if r.DatabaseID == dbID {
			return r
		}
	}
	return nil
}

// refTip returns a ref's tip and whether the ref exists.
func (r *Repo) refTip(ref string) (Oid, bool) {
	o, ok := r.refs[ref]
	return o, ok
}

// defaultTip returns the default branch tip and whether it exists.
func (r *Repo) defaultTip() (Oid, bool) {
	o, ok := r.refs["refs/heads/"+r.DefaultBranch]
	return o, ok
}

// setRef moves a ref and invalidates the git advertisement caches.
func (r *Repo) setRef(ref string, oid Oid) {
	r.refs[ref] = oid
	r.version++
}

// deleteRef removes a ref and invalidates the git advertisement caches.
func (r *Repo) deleteRef(ref string) {
	delete(r.refs, ref)
	r.version++
}

// sortedRefNames returns all ref names in sorted order.
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func sortStrings(s []string) {
	// Insertion sort: ref lists are tiny, and this avoids importing sort.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
