// merge.go: the shared pull-request merge engine — the three-way
// conflict check, the squash commit, and the changed-files diff. REST,
// GraphQL and MCP all merge through doMerge, so the three surfaces can
// never disagree about a pull request.
package main

import (
	"strings"
)

// pullFile is one changed file of a pull request.
type pullFile struct {
	path      string
	status    string // "added" | "removed" | "modified"
	additions int
	deletions int
}

// countLines counts the lines of a blob (a trailing newline does not
// add a phantom line).
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, c := range data {
		if c == '\n' {
			n++
		}
	}
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// diffTrees returns the changed files between two trees: paths whose
// blob differs, is new, or is gone.
func (s *server) diffTrees(repo *Repo, baseTree, headTree Oid) []pullFile {
	base := make(map[string]Oid)
	head := make(map[string]Oid)
	if !baseTree.isZero() {
		repo.flattenTree(baseTree, "", base)
	}
	if !headTree.isZero() {
		repo.flattenTree(headTree, "", head)
	}
	paths := make([]string, 0, len(base)+len(head))
	seen := make(map[string]bool)
	for p := range base {
		paths = append(paths, p)
		seen[p] = true
	}
	for p := range head {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	sortStrings(paths)
	var out []pullFile
	for _, p := range paths {
		b, inBase := base[p]
		h, inHead := head[p]
		switch {
		case inBase && !inHead:
			out = append(out, pullFile{path: p, status: "removed", deletions: s.blobLines(repo, b)})
		case !inBase && inHead:
			out = append(out, pullFile{path: p, status: "added", additions: s.blobLines(repo, h)})
		case b != h:
			out = append(out, pullFile{path: p, status: "modified", additions: s.blobLines(repo, h), deletions: s.blobLines(repo, b)})
		}
	}
	return out
}

func (s *server) blobLines(repo *Repo, oid Oid) int {
	o := repo.objects[oid]
	if o == nil {
		return 0
	}
	return countLines(o.data)
}

// pullBase returns the merge-base commit of a pull request (or zero
// when the branches share no history).
func (s *server) pullBase(repo *Repo, pull *Pull) Oid {
	headOid, ok1 := oidFromHex(pull.HeadSHA)
	baseOid, ok2 := oidFromHex(pull.BaseSHA)
	if !ok1 || !ok2 {
		return ZeroOid
	}
	// The base branch may have advanced since the pull was opened; the
	// merge-base is against the CURRENT base tip.
	if cur, ok := repo.refTip("refs/heads/" + pull.BaseRef); ok {
		baseOid = cur
	}
	if cur, ok := repo.refTip("refs/heads/" + pull.HeadRef); ok {
		headOid = cur
	}
	base, ok := repo.mergeBase(headOid, baseOid)
	if !ok {
		return ZeroOid
	}
	return base
}

// pullFiles lists the pull request's changed files (merge-base to head).
func (s *server) pullFiles(repo *Repo, pull *Pull) []pullFile {
	headOid, ok := oidFromHex(pull.HeadSHA)
	if !ok {
		return nil
	}
	if cur, ok := repo.refTip("refs/heads/" + pull.HeadRef); ok {
		headOid = cur
	}
	headTree, ok := repo.commitTree(headOid)
	if !ok {
		return nil
	}
	base := s.pullBase(repo, pull)
	var baseTree Oid
	if !base.isZero() {
		baseTree, _ = repo.commitTree(base)
	}
	return s.diffTrees(repo, baseTree, headTree)
}

// mergeConflict runs the three-way check: a path changed on both sides
// of the merge-base with different results is a conflict. It returns
// the conflicting paths (empty when the merge is clean).
func (s *server) mergeConflict(repo *Repo, pull *Pull) []string {
	headOid, ok1 := oidFromHex(pull.HeadSHA)
	baseOid, ok2 := oidFromHex(pull.BaseSHA)
	if !ok1 || !ok2 {
		return nil
	}
	if cur, ok := repo.refTip("refs/heads/" + pull.HeadRef); ok {
		headOid = cur
	}
	if cur, ok := repo.refTip("refs/heads/" + pull.BaseRef); ok {
		baseOid = cur
	}
	mergeBase, ok := repo.mergeBase(headOid, baseOid)
	if !ok {
		// Unrelated histories: every head-side file is "changed" against
		// an empty base; a conflict needs a path the base side also
		// touched, which is impossible against an empty tree.
		return nil
	}
	baseTree, _ := repo.commitTree(mergeBase)
	headTree, _ := repo.commitTree(headOid)
	curTree, _ := repo.commitTree(baseOid)
	baseFlat := make(map[string]Oid)
	headFlat := make(map[string]Oid)
	curFlat := make(map[string]Oid)
	repo.flattenTree(baseTree, "", baseFlat)
	repo.flattenTree(headTree, "", headFlat)
	repo.flattenTree(curTree, "", curFlat)
	var conflicts []string
	for path, headOid := range headFlat {
		baseOid, inBase := baseFlat[path]
		if inBase && headOid == baseOid {
			continue // unchanged on the head side
		}
		curOid, inCur := curFlat[path]
		if inBase && inCur && curOid == baseOid {
			continue // unchanged on the base side
		}
		if !inBase && !inCur {
			continue // added on the head side only
		}
		if inCur && curOid == headOid {
			continue // both sides reached the same content
		}
		conflicts = append(conflicts, path)
	}
	// Paths deleted on the head side but modified on the base side.
	for path, baseOid := range baseFlat {
		if _, inHead := headFlat[path]; inHead {
			continue
		}
		curOid, inCur := curFlat[path]
		if inCur && curOid != baseOid {
			conflicts = append(conflicts, path)
		}
	}
	sortStrings(conflicts)
	return conflicts
}

// pullMergeable reports whether a pull request can merge cleanly.
func (s *server) pullMergeable(repo *Repo, pull *Pull) bool {
	if pull.State != "open" {
		return false
	}
	return len(s.mergeConflict(repo, pull)) == 0
}

// doMerge squash-merges an open pull request onto its base branch. It
// returns the merge commit sha and HTTP 200 on success, or a status
// (404 unknown, 405 conflicted/already merged) and a message.
func (s *server) doMerge(repo *Repo, pull *Pull) (string, int, string) {
	if pull.State != "open" {
		return "", 405, "Pull request has already been merged or closed"
	}
	headOid, ok1 := oidFromHex(pull.HeadSHA)
	if !ok1 {
		return "", 405, "Pull request head is not a commit"
	}
	if cur, ok := repo.refTip("refs/heads/" + pull.HeadRef); ok {
		headOid = cur
	}
	baseTip, ok := repo.refTip("refs/heads/" + pull.BaseRef)
	if !ok {
		return "", 405, "Pull request base branch does not exist"
	}
	if len(s.mergeConflict(repo, pull)) > 0 {
		return "", 405, "Merge conflict: the pull request cannot be merged cleanly"
	}
	// Apply the head-side changes onto the base tip's tree.
	mergeBase, hasBase := repo.mergeBase(headOid, baseTip)
	var baseTree Oid
	if hasBase {
		baseTree, _ = repo.commitTree(mergeBase)
	}
	headTree, ok := repo.commitTree(headOid)
	if !ok {
		return "", 405, "Pull request head has no tree"
	}
	curTree, ok := repo.commitTree(baseTip)
	if !ok {
		return "", 405, "Pull request base has no tree"
	}
	baseFlat := make(map[string]Oid)
	headFlat := make(map[string]Oid)
	repo.flattenTree(baseTree, "", baseFlat)
	repo.flattenTree(headTree, "", headFlat)
	newTree := curTree
	// Deterministic order: sort the head-side paths.
	paths := make([]string, 0, len(headFlat))
	for p := range headFlat {
		paths = append(paths, p)
	}
	sortStrings(paths)
	for _, p := range paths {
		h := headFlat[p]
		if b, inBase := baseFlat[p]; inBase && b == h {
			continue // unchanged on the head side
		}
		newTree = repo.setTreePath(newTree, strings.Split(p, "/"), h)
	}
	// Deletions on the head side.
	delPaths := make([]string, 0)
	for p := range baseFlat {
		if _, inHead := headFlat[p]; !inHead {
			delPaths = append(delPaths, p)
		}
	}
	sortStrings(delPaths)
	for _, p := range delPaths {
		if t, ok := repo.deleteTreePath(newTree, strings.Split(p, "/")); ok {
			newTree = t
		}
	}
	login := s.store.viewerLogin
	message := pull.Title + "\n\n" + pull.Body
	if strings.TrimSpace(pull.Body) == "" {
		message = pull.Title
	}
	mergeOid := repo.putObject(objCommit, buildCommit(newTree, []Oid{baseTip}, login, login+"@forge.local", s.store.nowUnixSec, message))
	repo.setRef("refs/heads/"+pull.BaseRef, mergeOid)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = s.store.nowISO
	pull.MergeCommit = mergeOid.hex()
	pull.UpdatedAt = s.store.nowISO
	return mergeOid.hex(), 200, ""
}
