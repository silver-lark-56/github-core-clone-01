// gitobj.go: the git object model — hashing, commit/tree/tag parsing,
// tree construction, packfile reading and writing (with OFS/REF delta
// resolution on read; full objects only on write), reachability closure,
// merge-base and tree diff.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"strconv"
)

// hashObject returns the SHA-1 id of "<type> <len>\0<data>".
func hashObject(typ byte, data []byte) Oid {
	h := sha1.New()
	h.Write([]byte(objTypeName(typ)))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	var o Oid
	h.Sum(o[:0])
	return o
}

// putObject stores an object, returning its id. Call with mu held.
func (r *Repo) putObject(typ byte, data []byte) Oid {
	o := hashObject(typ, data)
	if _, ok := r.objects[o]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.objects[o] = &Obj{typ: typ, data: cp}
	}
	return o
}

// --- commit parsing ---

// parseCommit extracts a commit's tree and parents. The message is the
// bytes after the first blank line.
func parseCommit(data []byte) (tree Oid, parents []Oid, message string, ok bool) {
	rest := data
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return tree, parents, "", false
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if len(line) == 0 {
			message = string(rest)
			return tree, parents, message, true
		}
		if bytes.HasPrefix(line, []byte("tree ")) && len(line) == 45 {
			if o, good := oidFromHex(string(line[5:])); good {
				tree = o
			}
		} else if bytes.HasPrefix(line, []byte("parent ")) && len(line) == 47 {
			if o, good := oidFromHex(string(line[7:])); good {
				parents = append(parents, o)
			}
		}
		// author/committer/gpgsig lines are skipped; continuation lines
		// (starting with a space) belong to the previous header.
	}
	return tree, parents, "", false
}

// commitMessage returns a commit's message with one trailing newline
// trimmed (the REST surface compares messages exactly).
func commitMessage(data []byte) string {
	_, _, msg, ok := parseCommit(data)
	if !ok {
		return ""
	}
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	return msg
}

// buildCommit renders a commit object. parents may be empty (root commit).
func buildCommit(tree Oid, parents []Oid, authorName, authorEmail string, ts int64, message string) []byte {
	var b bytes.Buffer
	b.WriteString("tree ")
	b.WriteString(tree.hex())
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p.hex())
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(authorName)
	b.WriteString(" <")
	b.WriteString(authorEmail)
	b.WriteString("> ")
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteString(" +0000\n")
	b.WriteString("committer ")
	b.WriteString(authorName)
	b.WriteString(" <")
	b.WriteString(authorEmail)
	b.WriteString("> ")
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteString(" +0000\n")
	b.WriteByte('\n')
	b.WriteString(message)
	if len(message) == 0 || message[len(message)-1] != '\n' {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// --- tree parsing and construction ---

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string // "100644" or "40000"
	Name string
	Oid  Oid
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	rest := data
	for len(rest) > 0 {
		sp := bytes.IndexByte(rest, ' ')
		if sp < 0 {
			break
		}
		mode := string(rest[:sp])
		rest = rest[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+21 {
			break
		}
		name := string(rest[:nul])
		var o Oid
		copy(o[:], rest[nul+1:nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, Oid: o})
		rest = rest[nul+21:]
	}
	return out
}

// treeLess orders entries git's way: by name, directories as name+"/".
func treeLess(a, b TreeEntry) bool {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return an < bn
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return treeLess(sorted[i], sorted[j]) })
	var b bytes.Buffer
	for _, e := range sorted {
		b.WriteString(e.Mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		b.Write(e.Oid[:])
	}
	return b.Bytes()
}

// treeEntriesOf returns a tree object's entries (empty when missing).
func (r *Repo) treeEntriesOf(oid Oid) []TreeEntry {
	o := r.objects[oid]
	if o == nil || o.typ != objTree {
		return nil
	}
	return parseTree(o.data)
}

// findTreePath resolves segments in a tree, returning the blob oid or
// the zero oid when the path does not name a file.
func (r *Repo) findTreePath(treeOid Oid, segments []string) Oid {
	cur := treeOid
	for i, seg := range segments {
		found := false
		for _, e := range r.treeEntriesOf(cur) {
			if e.Name != seg {
				continue
			}
			if i == len(segments)-1 {
				if e.Mode == "40000" {
					return ZeroOid
				}
				return e.Oid
			}
			if e.Mode != "40000" {
				return ZeroOid
			}
			cur = e.Oid
			found = true
			break
		}
		if !found {
			return ZeroOid
		}
	}
	return ZeroOid
}

// findTreeDir resolves segments to a subtree oid (the root tree when
// segments is empty); ok is false when the path is not a directory.
func (r *Repo) findTreeDir(treeOid Oid, segments []string) (Oid, bool) {
	cur := treeOid
	for _, seg := range segments {
		found := false
		for _, e := range r.treeEntriesOf(cur) {
			if e.Name == seg && e.Mode == "40000" {
				cur = e.Oid
				found = true
				break
			}
		}
		if !found {
			return ZeroOid, false
		}
	}
	return cur, true
}

// setTreePath returns a new tree oid with segments set to blobOid,
// creating intermediate directories. The parent tree is empty when
// treeOid is zero.
func (r *Repo) setTreePath(treeOid Oid, segments []string, blobOid Oid) Oid {
	head := segments[0]
	rest := segments[1:]
	entries := r.treeEntriesOf(treeOid)
	kept := entries[:0]
	// Rebuild without the head entry (copy-on-write: entries aliases the
	// parsed slice, so build a fresh slice).
	kept = make([]TreeEntry, 0, len(entries)+1)
	var childOid Oid
	childIsDir := false
	for _, e := range entries {
		if e.Name == head {
			childOid = e.Oid
			childIsDir = e.Mode == "40000"
			continue
		}
		kept = append(kept, e)
	}
	if len(rest) == 0 {
		kept = append(kept, TreeEntry{Mode: "100644", Name: head, Oid: blobOid})
	} else {
		var childTree Oid
		if childIsDir {
			childTree = childOid
		}
		kept = append(kept, TreeEntry{Mode: "40000", Name: head, Oid: r.setTreePath(childTree, rest, blobOid)})
	}
	return r.putObject(objTree, buildTree(kept))
}

// deleteTreePath returns a new tree oid with segments removed, pruning
// directories left empty; ok is false when the path did not exist.
func (r *Repo) deleteTreePath(treeOid Oid, segments []string) (Oid, bool) {
	head := segments[0]
	rest := segments[1:]
	entries := r.treeEntriesOf(treeOid)
	kept := make([]TreeEntry, 0, len(entries))
	changed := false
	for _, e := range entries {
		if e.Name != head {
			kept = append(kept, e)
			continue
		}
		changed = true
		if len(rest) == 0 {
			continue // drop the entry
		}
		if e.Mode != "40000" {
			return ZeroOid, false
		}
		child, ok := r.deleteTreePath(e.Oid, rest)
		if !ok {
			return ZeroOid, false
		}
		if len(r.treeEntriesOf(child)) > 0 {
			kept = append(kept, TreeEntry{Mode: "40000", Name: head, Oid: child})
		}
	}
	if !changed {
		return ZeroOid, false
	}
	return r.putObject(objTree, buildTree(kept)), true
}

// flattenTree walks a tree into path -> blob oid.
func (r *Repo) flattenTree(treeOid Oid, prefix string, out map[string]Oid) {
	for _, e := range r.treeEntriesOf(treeOid) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			r.flattenTree(e.Oid, path, out)
		} else {
			out[path] = e.Oid
		}
	}
}

// commitTree returns a commit's tree oid.
func (r *Repo) commitTree(commitOid Oid) (Oid, bool) {
	o := r.objects[commitOid]
	if o == nil || o.typ != objCommit {
		return ZeroOid, false
	}
	tree, _, _, ok := parseCommit(o.data)
	return tree, ok
}

// peel follows tag objects to the underlying non-tag object.
func (r *Repo) peel(oid Oid) Oid {
	for {
		o := r.objects[oid]
		if o == nil || o.typ != objTag {
			return oid
		}
		target, ok := parseTagTarget(o.data)
		if !ok {
			return oid
		}
		oid = target
	}
}

// parseTagTarget extracts the "object <hex>" line of a tag object.
func parseTagTarget(data []byte) (Oid, bool) {
	if !bytes.HasPrefix(data, []byte("object ")) || len(data) < 48 {
		return ZeroOid, false
	}
	return oidFromHex(string(data[7:47]))
}

// --- ancestry ---

// isAncestorOf reports whether anc is reachable from desc (following
// commit parents, peeling tags first). anc == desc counts.
func (r *Repo) isAncestorOf(anc, desc Oid) bool {
	desc = r.peel(desc)
	if anc == desc {
		return true
	}
	seen := make(map[Oid]bool)
	stack := []Oid{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.objects[cur]
		if o == nil || o.typ != objCommit {
			continue
		}
		_, parents, _, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		for _, p := range parents {
			if p == anc {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// mergeBase finds a common ancestor of a and b (the first found walking
// from a through b's ancestor set; exact for the linear histories the
// suite builds).
func (r *Repo) mergeBase(a, b Oid) (Oid, bool) {
	a = r.peel(a)
	b = r.peel(b)
	ancB := make(map[Oid]bool)
	stack := []Oid{b}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancB[cur] {
			continue
		}
		ancB[cur] = true
		o := r.objects[cur]
		if o == nil || o.typ != objCommit {
			continue
		}
		_, parents, _, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		stack = append(stack, parents...)
	}
	seen := make(map[Oid]bool)
	stack = []Oid{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancB[cur] {
			return cur, true
		}
		o := r.objects[cur]
		if o == nil || o.typ != objCommit {
			continue
		}
		_, parents, _, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		stack = append(stack, parents...)
	}
	return ZeroOid, false
}

// commitHistory lists commits reachable from tip, newest first
// (pre-order depth-first walk over all parents).
func (r *Repo) commitHistory(tip Oid, limit int) []Oid {
	var out []Oid
	seen := make(map[Oid]bool)
	stack := []Oid{tip}
	for len(stack) > 0 && len(out) < limit {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.objects[cur]
		if o == nil || o.typ != objCommit {
			continue
		}
		out = append(out, cur)
		_, parents, _, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		// Push in reverse so the first parent is visited first.
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	return out
}

// --- reachability closure for fetch ---

// closureResult is what a fetch sends: the object set plus the
// shallow/unshallow boundary lines.
type closureResult struct {
	objects   map[Oid]bool
	shallow   []string
	unshallow []string
}

// fetchClosure computes the objects a fetch must send for wants, minus
// what the client already has (haves, full closure), limited to depth
// generations when depth > 0 (0 = unlimited).
//
// Included commits are those at distance < depth from a want; a commit
// at distance depth-1 with parents is a shallow boundary. A commit the
// client named shallow that is now included with its parents included
// is unshallow.
func (r *Repo) fetchClosure(wants, haves []Oid, clientShallow []Oid, depth int) closureResult {
	// Closure of haves: everything the client already holds — but a
	// shallow client holds only the shallow commit itself and its tree,
	// NOT its parents. Descending past a client-shallow commit would
	// subtract objects the client has never seen (a deepen fetch would
	// then deliver nothing).
	shallowSet := make(map[Oid]bool, len(clientShallow))
	for _, oid := range clientShallow {
		shallowSet[oid] = true
	}
	held := make(map[Oid]bool)
	for _, h := range haves {
		r.collectHeldClosure(h, shallowSet, held)
	}

	included := make(map[Oid]bool) // commits included by depth
	boundary := make(map[Oid]bool) // included commits whose parents are cut
	type item struct {
		oid   Oid
		depth int
	}
	// BFS over commits from wants.
	queue := []item{}
	seenCommit := make(map[Oid]bool)
	for _, w := range wants {
		t := r.peel(w)
		// A wanted tag object itself is always included.
		if w != t {
			included[w] = true
		}
		if !seenCommit[t] {
			seenCommit[t] = true
			queue = append(queue, item{t, 0})
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		o := r.objects[cur.oid]
		if o == nil || o.typ != objCommit {
			continue
		}
		included[cur.oid] = true
		_, parents, _, ok := parseCommit(o.data)
		if !ok {
			continue
		}
		atLimit := depth > 0 && cur.depth+1 >= depth
		if atLimit {
			if len(parents) > 0 {
				boundary[cur.oid] = true
			}
			continue
		}
		for _, p := range parents {
			if !seenCommit[p] {
				seenCommit[p] = true
				queue = append(queue, item{p, cur.depth + 1})
			}
		}
	}

	// Expand included commits to their trees and blobs — WITHOUT
	// following parents: the depth limit already chose which commits
	// go out, and collectClosure would walk the whole history back.
	send := make(map[Oid]bool)
	for oid := range included {
		r.collectTreeClosure(oid, send)
	}
	// Drop what the client holds.
	for oid := range held {
		delete(send, oid)
	}
	// Shallow boundaries: included commits at the cut, excluding ones
	// the client already holds (they stay shallow on the client side
	// only if they were already shallow there; git reports the new
	// boundary regardless — but a boundary the client holds needs no
	// line, since its parents are already absent client-side... in
	// practice the checks' boundaries are always newly sent commits).
	var shallow []string
	for oid := range boundary {
		if send[oid] {
			shallow = append(shallow, oid.hex())
		}
	}
	sort.Strings(shallow)
	// Unshallow: client-shallow commits now included with parents
	// included (i.e. included and not a boundary).
	var unshallow []string
	for _, oid := range clientShallow {
		if included[oid] && !boundary[oid] {
			unshallow = append(unshallow, oid.hex())
		}
	}
	sort.Strings(unshallow)
	return closureResult{objects: send, shallow: shallow, unshallow: unshallow}
}

// collectClosure adds oid and everything it references (tag targets,
// commit trees and blobs, recursively) to out.
func (r *Repo) collectClosure(oid Oid, out map[Oid]bool) {
	stack := []Oid{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		o := r.objects[cur]
		if o == nil {
			continue
		}
		out[cur] = true
		switch o.typ {
		case objTag:
			if target, ok := parseTagTarget(o.data); ok {
				stack = append(stack, target)
			}
		case objCommit:
			tree, parents, _, ok := parseCommit(o.data)
			if !ok {
				continue
			}
			stack = append(stack, tree)
			stack = append(stack, parents...)
		case objTree:
			for _, e := range parseTree(o.data) {
				stack = append(stack, e.Oid)
			}
		}
	}
}

// collectHeldClosure adds everything the client holds under oid: the
// full closure, except that traversal stops at commits in the
// client-shallow set (their parents were never sent to that client).
func (r *Repo) collectHeldClosure(oid Oid, shallow map[Oid]bool, out map[Oid]bool) {
	stack := []Oid{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		o := r.objects[cur]
		if o == nil {
			continue
		}
		out[cur] = true
		switch o.typ {
		case objTag:
			if target, ok := parseTagTarget(o.data); ok {
				stack = append(stack, target)
			}
		case objCommit:
			tree, parents, _, ok := parseCommit(o.data)
			if !ok {
				continue
			}
			stack = append(stack, tree)
			if !shallow[cur] {
				stack = append(stack, parents...)
			}
		case objTree:
			for _, e := range parseTree(o.data) {
				stack = append(stack, e.Oid)
			}
		}
	}
}

// collectTreeClosure adds oid and its tree/blob closure to out: a tag
// follows its target, a commit follows its tree only (never its
// parents), a tree follows its entries. This is the expansion a
// depth-limited fetch needs — the commit set was already chosen by
// the BFS in fetchClosure.
func (r *Repo) collectTreeClosure(oid Oid, out map[Oid]bool) {
	stack := []Oid{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		o := r.objects[cur]
		if o == nil {
			continue
		}
		out[cur] = true
		switch o.typ {
		case objTag:
			if target, ok := parseTagTarget(o.data); ok {
				stack = append(stack, target)
			}
		case objCommit:
			tree, _, _, ok := parseCommit(o.data)
			if !ok {
				continue
			}
			stack = append(stack, tree)
		case objTree:
			for _, e := range parseTree(o.data) {
				stack = append(stack, e.Oid)
			}
		}
	}
}

// commonHaves returns the haves that are ancestors of any want
// (the ACK set for fetch negotiation).
func (r *Repo) commonHaves(wants, haves []Oid) []string {
	if len(haves) == 0 {
		return nil
	}
	ancestors := make(map[Oid]bool)
	for _, w := range wants {
		t := r.peel(w)
		stack := []Oid{t}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if ancestors[cur] {
				continue
			}
			ancestors[cur] = true
			o := r.objects[cur]
			if o == nil || o.typ != objCommit {
				continue
			}
			_, parents, _, ok := parseCommit(o.data)
			if !ok {
				continue
			}
			stack = append(stack, parents...)
		}
	}
	var acks []string
	for _, h := range haves {
		if ancestors[h] {
			acks = append(acks, h.hex())
		}
	}
	sort.Strings(acks)
	return acks
}

// --- packfile writing ---

// writePack renders objects as a packfile version 2: full objects only,
// zlib at best speed, SHA-1 trailer. Order is sorted by oid for
// determinism.
func writePack(objects map[Oid]*Obj, ids []Oid) []byte {
	sorted := make([]Oid, len(ids))
	copy(sorted, ids)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})
	var b bytes.Buffer
	b.WriteString("PACK")
	binary.Write(&b, binary.BigEndian, uint32(2))
	binary.Write(&b, binary.BigEndian, uint32(len(sorted)))
	for _, id := range sorted {
		o := objects[id]
		if o == nil {
			continue
		}
		writePackEntry(&b, o.typ, o.data)
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

func writePackEntry(b *bytes.Buffer, typ byte, data []byte) {
	// Type/size varint: first byte holds type (bits 4-6) and the low 4
	// size bits; continuation bytes hold 7 size bits each, little-endian.
	size := uint64(len(data))
	first := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	b.WriteByte(first)
	for size > 0 {
		c := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			c |= 0x80
		}
		b.WriteByte(c)
	}
	w, _ := zlib.NewWriterLevel(b, zlib.BestSpeed)
	w.Write(data)
	w.Close()
}

// --- packfile reading ---

var errPack = errors.New("bad packfile")

// readPack parses a packfile, resolving OFS/REF deltas against the
// pack's own objects and the repository's. It returns the new objects
// (oid -> object); on any error the pack is rejected whole.
func (r *Repo) readPack(pack []byte) (map[Oid]*Obj, error) {
	if len(pack) < 32 {
		return nil, errPack
	}
	if string(pack[:4]) != "PACK" {
		return nil, errPack
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, errPack
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	// Verify the trailer checksum.
	want := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(want[:], pack[len(pack)-20:]) {
		return nil, errPack
	}
	out := make(map[Oid]*Obj, count)
	// Offsets of entries already parsed in this pack (for OFS_DELTA).
	offsets := make(map[int]Oid)
	pos := 12
	bodyEnd := len(pack) - 20
	for i := uint32(0); i < count; i++ {
		if pos >= bodyEnd {
			return nil, errPack
		}
		entryStart := pos
		typ, size, n, ok := readPackHeader(pack, pos)
		if !ok {
			return nil, errPack
		}
		pos += n
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			data, used, err := inflate(pack[pos:bodyEnd], size)
			if err != nil {
				return nil, err
			}
			pos += used
			oid := hashObject(typ, data)
			out[oid] = &Obj{typ: typ, data: data}
			offsets[entryStart] = oid
		case 6: // OFS_DELTA
			off, m, ok := readOffset(pack, pos)
			if !ok {
				return nil, errPack
			}
			pos += m
			basePos := entryStart - off
			baseOid, ok := offsets[basePos]
			if !ok {
				return nil, errPack
			}
			base := out[baseOid]
			if base == nil {
				return nil, errPack
			}
			delta, used, err := inflate(pack[pos:bodyEnd], size)
			if err != nil {
				return nil, err
			}
			pos += used
			data, err := applyDelta(base.data, delta)
			if err != nil {
				return nil, err
			}
			oid := hashObject(base.typ, data)
			out[oid] = &Obj{typ: base.typ, data: data}
			offsets[entryStart] = oid
		case 7: // REF_DELTA
			if pos+20 > bodyEnd {
				return nil, errPack
			}
			var baseOid Oid
			copy(baseOid[:], pack[pos:pos+20])
			pos += 20
			base := out[baseOid]
			if base == nil {
				base = r.objects[baseOid]
			}
			if base == nil {
				return nil, errPack
			}
			delta, used, err := inflate(pack[pos:bodyEnd], size)
			if err != nil {
				return nil, err
			}
			pos += used
			data, err := applyDelta(base.data, delta)
			if err != nil {
				return nil, err
			}
			oid := hashObject(base.typ, data)
			out[oid] = &Obj{typ: base.typ, data: data}
			offsets[entryStart] = oid
		default:
			return nil, errPack
		}
	}
	return out, nil
}

// readPackHeader decodes a pack entry header: type, size, bytes used.
func readPackHeader(b []byte, pos int) (typ byte, size uint64, used int, ok bool) {
	if pos >= len(b) {
		return 0, 0, 0, false
	}
	c := b[pos]
	typ = (c >> 4) & 0x7
	size = uint64(c & 0x0f)
	shift := uint(4)
	used = 1
	for c&0x80 != 0 {
		if pos+used >= len(b) {
			return 0, 0, 0, false
		}
		c = b[pos+used]
		used++
		size |= uint64(c&0x7f) << shift
		shift += 7
	}
	return typ, size, used, true
}

// readOffset decodes an OFS_DELTA negative offset.
func readOffset(b []byte, pos int) (off int, used int, ok bool) {
	if pos >= len(b) {
		return 0, 0, false
	}
	c := b[pos]
	off = int(c & 0x7f)
	used = 1
	for c&0x80 != 0 {
		if pos+used >= len(b) {
			return 0, 0, false
		}
		c = b[pos+used]
		used++
		off = ((off + 1) << 7) | int(c&0x7f)
	}
	return off, used, true
}

// inflate decompresses one zlib stream starting at b[0]; size is the
// expected output size (a sanity bound). It returns the data and the
// number of input bytes consumed. bytes.Reader implements io.ByteReader,
// so the zlib/flate readers consume exactly the stream's bytes and the
// remainder length is exact.
func inflate(b []byte, size uint64) ([]byte, int, error) {
	if size > 64<<20 {
		return nil, 0, errPack
	}
	br := bytes.NewReader(b)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(zr, int64(size)+1))
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	if uint64(len(data)) != size {
		return nil, 0, errPack
	}
	return data, len(b) - br.Len(), nil
}

// applyDelta applies a git delta to base, returning the result.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (uint64, bool) {
		var v uint64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, false
			}
			c := delta[pos]
			pos++
			v |= uint64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, true
			}
			if shift > 63 {
				return 0, false
			}
		}
	}
	baseSize, ok := readVarint()
	if !ok || baseSize != uint64(len(base)) {
		return nil, errPack
	}
	resultSize, ok := readVarint()
	if !ok || resultSize > 64<<20 {
		return nil, errPack
	}
	out := make([]byte, 0, resultSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base: bits 0-3 select offset bytes, 4-6 size bytes.
			var off, size uint64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errPack
					}
					off |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errPack
					}
					size |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > uint64(len(base)) {
				return nil, errPack
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			n := int(op)
			if pos+n > len(delta) {
				return nil, errPack
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, errPack
		}
	}
	if uint64(len(out)) != resultSize {
		return nil, errPack
	}
	return out, nil
}
