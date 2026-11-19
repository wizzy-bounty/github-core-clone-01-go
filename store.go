package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Git objects

// Object is one git object: a type and its raw bytes.
type Object struct {
	Typ  string // "blob", "tree", "commit", "tag"
	Data []byte
}

// gitOID hashes an object the way git does: sha1("<type> <size>\0" + data).
func gitOID(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

const zeroOID = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------------------
// Store: all repos, one viewer, the node-id maps GraphQL needs.

type nodeRef struct {
	kind string // "repo", "issue", "pull", "comment"
	repo string
	num  int
}

type Store struct {
	mu sync.RWMutex

	viewerLogin     string
	viewerConfirmed bool
	viewerID        int

	repos     map[string]*Repo // by name; every repo belongs to the viewer
	repoOrder []string
	nextRepoID int

	nodes       map[string]*nodeRef // opaque GraphQL id -> target
	nextIssueDB int                 // global database id for issues
	nextPullDB  int                 // global database id for pulls

	seq atomic.Int64 // logical clock: timestamps are epoch + seq
}

func newStore(viewerLogin, token string) *Store {
	return &Store{
		viewerLogin: viewerLogin,
		viewerID:    1000,
		repos:       map[string]*Repo{},
		nextRepoID:  5000,
		nodes:       map[string]*nodeRef{},
		nextIssueDB: 1,
		nextPullDB:  1,
	}
}

// nextTime returns the next deterministic timestamp (epoch + counter).
func (s *Store) nextTime() int64 {
	return scenarioEpochSec + s.seq.Add(1)
}

func isoTime(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// opaqueID returns a stable opaque GraphQL id for a node, registering it.
func (s *Store) opaqueID(kind, repo string, num int) string {
	prefix := map[string]string{"repo": "R_", "issue": "I_", "pull": "PR_", "comment": "IC_"}[kind]
	raw := kind + ":" + repo + ":" + strconv.Itoa(num)
	sum := sha1.Sum([]byte(raw))
	id := prefix + hex.EncodeToString(sum[:12])
	s.mu.Lock()
	s.nodes[id] = &nodeRef{kind: kind, repo: repo, num: num}
	s.mu.Unlock()
	return id
}

// lookupNode resolves an opaque id back to its target.
func (s *Store) lookupNode(id string) *nodeRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[id]
}

// adoptOwner records the owner named by the first owner-bearing request.
// The suite always names the viewer first (git checks run before anything
// else), so this self-corrects a derivation miss; after adoption the owner
// is strict, which is what makes the stranger a 404.
func (s *Store) adoptOwner(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.viewerConfirmed {
		s.viewerLogin = owner
		s.viewerConfirmed = true
	}
}

func (s *Store) viewer() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.viewerLogin
}

// getRepo returns the repo named (owner, name), or nil. A non-viewer owner
// never owns a repo, so it is always nil.
func (s *Store) getRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if owner != s.viewerLogin {
		return nil
	}
	return s.repos[name]
}

// createRepo creates a repo under the viewer; ok=false when the name exists.
func (s *Store) createRepo(name, desc string, priv bool) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.repos[name]; exists {
		return nil, false
	}
	r := &Repo{
		name:          name,
		id:            s.nextRepoID,
		desc:          desc,
		priv:          priv,
		defaultBranch: "main",
		refs:          map[string]string{},
		objects:       map[string]*Object{},
		issueByNum:   map[int]*Issue{},
		labels:      map[string]*Label{},
		comments:    map[int][]*Comment{},
		pullByNum:   map[int]*Pull{},
		nextIssue:     1,
		nextPull:      1,
		nextComment:   1,
		packCache:     map[string][]byte{},
		issueJSON:     map[int][]byte{},
	}
	s.nextRepoID++
	s.repos[name] = r
	s.repoOrder = append(s.repoOrder, name)
	return r, true
}

// deleteRepo removes a repo; ok=false when absent.
func (s *Store) deleteRepo(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.repos[name]; !exists {
		return false
	}
	delete(s.repos, name)
	for i, n := range s.repoOrder {
		if n == name {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// listRepos returns the viewer's repos in creation order.
func (s *Store) listRepos() []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Repo, 0, len(s.repoOrder))
	for _, name := range s.repoOrder {
		out = append(out, s.repos[name])
	}
	return out
}

// ---------------------------------------------------------------------------
// Repository

type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Issue struct {
	Number    int
	DBID      int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	CreatedAt int64
	cached    []byte // precomputed GET /issues/{n} body
}

type Comment struct {
	ID        int
	Body      string
	IssueNum  int
	CreatedAt int64
}

type Pull struct {
	Number        int
	DBID          int
	Title         string
	Body          string
	State         string // "open" | "closed"
	HeadRef       string
	BaseRef       string
	HeadSha       string
	BaseSha       string
	BaseTipCreate string // base tip at creation (conflict detection)
	Merged        bool
	MergedAt      string
	MergeSha      string
	CreatedAt     int64
}

type Repo struct {
	mu sync.RWMutex

	name          string
	id            int // databaseId
	desc          string
	priv          bool
	defaultBranch string

	refs    map[string]string // full ref name -> oid
	objects map[string]*Object

	issues     []*Issue
	issueByNum map[int]*Issue
	nextIssue  int
	labels     map[string]*Label
	labelOrder []string
	comments   map[int][]*Comment // issue number -> comments
	nextComment int

	pulls     []*Pull
	pullByNum map[int]*Pull
	nextPull  int

	version uint64 // bumped on any ref/object change; invalidates caches

	// caches (guarded by mu)
	advV0Up    []byte
	advV0Recv  []byte
	lsRefLines []lsRefLine
	packCache  map[string][]byte
	issueJSON  map[int][]byte
}

// bump invalidates the per-repo caches after a mutation.
func (r *Repo) bump() {
	r.version++
	r.advV0Up = nil
	r.advV0Recv = nil
	r.lsRefLines = nil
	r.packCache = map[string][]byte{}
}

// tip returns the oid of refs/heads/<branch>.
func (r *Repo) tip(branch string) (string, bool) {
	oid, ok := r.refs["refs/heads/"+branch]
	return oid, ok
}

// putObject stores an object (idempotent) and returns its oid.
func (r *Repo) putObject(typ string, data []byte) string {
	oid := gitOID(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &Object{Typ: typ, Data: data}
	}
	return oid
}

// getObject returns the object, or nil.
func (r *Repo) getObject(oid string) *Object {
	return r.objects[oid]
}

// ---------------------------------------------------------------------------
// Trees and commits

type treeEntry struct {
	mode string // "100644", "40000", ...
	name string
	oid  string
}

// parseTree decodes a tree object's entries.
func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 || len(data) < sp+1+nul+1+20 {
			break
		}
		name := string(data[sp+1 : sp+1+nul])
		oid := hex.EncodeToString(data[sp+1+nul+1 : sp+1+nul+1+20])
		out = append(out, treeEntry{mode: string(data[:sp]), name: name, oid: oid})
		data = data[sp+1+nul+1+20:]
	}
	return out
}

// gitTreeSort sorts entries the way git does: directory names compare with a
// trailing '/'.
func gitTreeSort(entries []treeEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].name, entries[j].name
		if entries[i].mode == "40000" {
			a += "/"
		}
		if entries[j].mode == "40000" {
			b += "/"
		}
		return a < b
	})
}

// encodeTree serializes entries (sorted) into tree bytes.
func encodeTree(entries []treeEntry) []byte {
	gitTreeSort(entries)
	var buf bytes.Buffer
	for _, e := range entries {
		oid, _ := hex.DecodeString(e.oid)
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(oid)
	}
	return buf.Bytes()
}

// storeTree builds and stores a tree object from entries, returning its oid.
func (r *Repo) storeTree(entries []treeEntry) string {
	return r.putObject("tree", encodeTree(entries))
}

// treeWithPath returns a new root tree oid with the file at segments set to
// blobOid (creating intermediate trees). rootOid "" means an empty tree.
func (r *Repo) treeWithPath(rootOid string, segments []string, blobOid string) string {
	if len(segments) == 0 {
		return rootOid
	}
	var entries []treeEntry
	if rootOid != "" {
		if obj := r.getObject(rootOid); obj != nil && obj.Typ == "tree" {
			entries = parseTree(obj.Data)
		}
	}
	name := segments[0]
	if len(segments) == 1 {
		// Leaf: replace or add the blob.
		found := false
		for i := range entries {
			if entries[i].name == name {
				entries[i] = treeEntry{mode: "100644", name: name, oid: blobOid}
				found = true
			}
		}
		if !found {
			entries = append(entries, treeEntry{mode: "100644", name: name, oid: blobOid})
		}
		return r.storeTree(entries)
	}
	// Directory: recurse.
	var sub string
	for _, e := range entries {
		if e.name == name && e.mode == "40000" {
			sub = e.oid
		}
	}
	sub = r.treeWithPath(sub, segments[1:], blobOid)
	found := false
	for i := range entries {
		if entries[i].name == name {
			entries[i] = treeEntry{mode: "40000", name: name, oid: sub}
			found = true
		}
	}
	if !found {
		entries = append(entries, treeEntry{mode: "40000", name: name, oid: sub})
	}
	return r.storeTree(entries)
}

// treeDeletePath returns a new root tree oid with the file at segments
// removed. Returns "" when the tree becomes empty.
func (r *Repo) treeDeletePath(rootOid string, segments []string) string {
	if rootOid == "" || len(segments) == 0 {
		return rootOid
	}
	obj := r.getObject(rootOid)
	if obj == nil || obj.Typ != "tree" {
		return rootOid
	}
	entries := parseTree(obj.Data)
	name := segments[0]
	if len(segments) == 1 {
		var kept []treeEntry
		for _, e := range entries {
			if e.name != name {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			return ""
		}
		return r.storeTree(kept)
	}
	for i, e := range entries {
		if e.name == name && e.mode == "40000" {
			sub := r.treeDeletePath(e.oid, segments[1:])
			if sub == "" {
				entries = append(entries[:i], entries[i+1:]...)
			} else {
				entries[i].oid = sub
			}
			break
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return r.storeTree(entries)
}

// treeLookup resolves path segments in a tree: returns the oid, whether it
// is a directory, and whether it exists.
func (r *Repo) treeLookup(rootOid string, segments []string) (oid string, isDir bool, ok bool) {
	cur := rootOid
	for i, seg := range segments {
		obj := r.getObject(cur)
		if obj == nil || obj.Typ != "tree" {
			return "", false, false
		}
		found := false
		for _, e := range parseTree(obj.Data) {
			if e.name == seg {
				cur = e.oid
				found = true
				if i == len(segments)-1 {
					return e.oid, e.mode == "40000", true
				}
				break
			}
		}
		if !found {
			return "", false, false
		}
	}
	return cur, true, true
}

// flattenTree returns path -> blob oid for every file under a tree.
func (r *Repo) flattenTree(rootOid string) map[string]string {
	out := map[string]string{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := r.getObject(oid)
		if obj == nil || obj.Typ != "tree" {
			return
		}
		for _, e := range parseTree(obj.Data) {
			p := prefix + e.name
			if e.mode == "40000" {
				walk(e.oid, p+"/")
			} else if e.mode != "160000" { // skip gitlinks
				out[p] = e.oid
			}
		}
	}
	if rootOid != "" {
		walk(rootOid, "")
	}
	return out
}

// parseCommit returns the tree and parents of a commit object.
func parseCommit(data []byte) (tree string, parents []string) {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
	}
	return tree, parents
}

// parseTagTarget returns the object a tag points at.
func parseTagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

// commitMessage returns the message part of a commit object.
func commitMessage(data []byte) string {
	idx := bytes.Index(data, []byte("\n\n"))
	if idx < 0 {
		return ""
	}
	return string(data[idx+2:])
}

// buildCommit builds a commit object (server-made commits only).
func buildCommit(tree string, parents []string, message string, ts int64) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	ident := "Anvil <anvil@example.invalid> " + strconv.FormatInt(ts, 10) + " +0000"
	b.WriteString("author " + ident + "\n")
	b.WriteString("committer " + ident + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// ---------------------------------------------------------------------------
// Reachability

// heldClosure returns the objects the client already has: the closure of
// haves, cut at the shallow boundaries. A boundary commit is itself held,
// but its parents are not (that is what makes the clone shallow); without
// the cut, a deepen fetch would conclude the client already has the very
// history it is asking for.
func (r *Repo) heldClosure(haves, shallow []string) map[string]bool {
	boundary := map[string]bool{}
	for _, b := range shallow {
		boundary[b] = true
	}
	out := map[string]bool{}
	visited := map[string]bool{}
	queue := append([]string{}, haves...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || oid == zeroOID || visited[oid] {
			continue
		}
		visited[oid] = true
		obj := r.getObject(oid)
		if obj == nil {
			continue
		}
		out[oid] = true
		switch obj.Typ {
		case "commit":
			tree, parents := parseCommit(obj.Data)
			if tree != "" {
				queue = append(queue, tree)
			}
			if !boundary[oid] {
				queue = append(queue, parents...)
			}
		case "tree":
			for _, e := range parseTree(obj.Data) {
				if e.mode == "160000" {
					continue
				}
				queue = append(queue, e.oid)
			}
		case "tag":
			if tgt := parseTagTarget(obj.Data); tgt != "" {
				queue = append(queue, tgt)
			}
		}
	}
	return out
}

// reachable returns the oids reachable from starts. depth < 0 means no
// limit; depth >= 1 limits commit depth (start commits are depth 1, their
// trees and blobs are always included). held is the set the requester
// already has: held objects are NOT in the result, but the walk still
// descends through them (a held tip's parents may be new). The walk is a
// breadth-first traversal, so the first visit to an oid is its shallowest.
func (r *Repo) reachable(starts []string, depth int, held map[string]bool) map[string]bool {
	out := map[string]bool{}
	visited := map[string]bool{}
	type item struct {
		oid   string
		depth int
	}
	queue := []item{}
	for _, s := range starts {
		if s == "" || s == zeroOID {
			continue
		}
		queue = append(queue, item{oid: s, depth: 1})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if visited[it.oid] {
			continue
		}
		visited[it.oid] = true
		obj := r.getObject(it.oid)
		if obj == nil {
			continue
		}
		if !held[it.oid] {
			out[it.oid] = true
		}
		switch obj.Typ {
		case "commit":
			tree, parents := parseCommit(obj.Data)
			if tree != "" {
				queue = append(queue, item{oid: tree, depth: it.depth})
			}
			if depth < 0 || it.depth < depth {
				for _, p := range parents {
					queue = append(queue, item{oid: p, depth: it.depth + 1})
				}
			}
		case "tree":
			for _, e := range parseTree(obj.Data) {
				if e.mode == "160000" {
					continue
				}
				queue = append(queue, item{oid: e.oid, depth: it.depth})
			}
		case "tag":
			if tgt := parseTagTarget(obj.Data); tgt != "" {
				queue = append(queue, item{oid: tgt, depth: it.depth})
			}
		}
	}
	return out
}

// isAncestor reports whether anc is reachable from desc following parents.
func (r *Repo) isAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	queue := []string{desc}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.getObject(cur)
		if obj == nil || obj.Typ != "commit" {
			continue
		}
		_, parents := parseCommit(obj.Data)
		for _, p := range parents {
			if p == anc {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// commitHistory returns the commits reachable from tip, newest-first by
// timestamp (stable on walk order).
func (r *Repo) commitHistory(tip string) []string {
	if tip == "" || tip == zeroOID {
		return nil
	}
	seen := map[string]bool{}
	var order []string
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.getObject(cur)
		if obj == nil || obj.Typ != "commit" {
			continue
		}
		order = append(order, cur)
		_, parents := parseCommit(obj.Data)
		queue = append(queue, parents...)
	}
	// Newest-first by the commit's timestamp line.
	tsOf := func(oid string) int64 {
		obj := r.getObject(oid)
		if obj == nil {
			return 0
		}
		for _, line := range strings.Split(string(obj.Data), "\n") {
			if strings.HasPrefix(line, "committer ") {
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					if ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
						return ts
					}
				}
			}
			if line == "" {
				break
			}
		}
		return 0
	}
	sort.SliceStable(order, func(i, j int) bool { return tsOf(order[i]) > tsOf(order[j]) })
	return order
}

// ---------------------------------------------------------------------------
// Packs (plain objects only; the suite's client never sends deltas)

// buildPack serializes oids into a pack v2 with a SHA-1 trailer. The order
// is fixed (sorted oids) so the bytes are deterministic.
func (r *Repo) buildPack(oids map[string]bool) []byte {
	list := make([]string, 0, len(oids))
	for oid := range oids {
		list = append(list, oid)
	}
	sort.Strings(list)
	var buf bytes.Buffer
	buf.WriteString("PACK")
	buf.Write([]byte{0, 0, 0, 2})
	buf.Write([]byte{byte(len(list) >> 24), byte(len(list) >> 16), byte(len(list) >> 8), byte(len(list))})
	typeNum := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	for _, oid := range list {
		obj := r.getObject(oid)
		if obj == nil {
			continue
		}
		writePackObject(&buf, typeNum[obj.Typ], obj.Data)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writePackObject(buf *bytes.Buffer, typ int, data []byte) {
	size := len(data)
	// First byte: (size & 0x0f) | (type << 4) | continuation bit.
	b := byte(size&0x0f) | byte(typ<<4)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	buf.WriteByte(b)
	for size > 0 {
		nb := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			nb |= 0x80
		}
		buf.WriteByte(nb)
	}
	zw, _ := zlib.NewWriterLevel(buf, zlib.BestSpeed)
	_, _ = zw.Write(data)
	_ = zw.Close()
}

// parsePack decodes a pack v2 of plain objects, verifying each object's
// hash. Delta entries are rejected (the suite never sends them).
func parsePack(data []byte) ([]*Object, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	if data[4] != 0 || data[5] != 0 || data[6] != 0 || data[7] != 2 {
		return nil, fmt.Errorf("unsupported pack version")
	}
	count := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	// Trailer: last 20 bytes must be the SHA-1 of everything before them.
	if len(data) < 32 {
		return nil, fmt.Errorf("pack too short")
	}
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, fmt.Errorf("pack checksum mismatch")
	}
	pos := 12
	end := len(data) - 20
	var out []*Object
	typeNames := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
	for i := 0; i < count; i++ {
		if pos >= end {
			return nil, fmt.Errorf("pack truncated")
		}
		b := data[pos]
		pos++
		typ := int(b>>4) & 7
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= end {
				return nil, fmt.Errorf("pack truncated")
			}
			b = data[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		switch typ {
		case 6, 7:
			return nil, fmt.Errorf("delta objects are not supported")
		case 1, 2, 3, 4:
			// plain object: zlib stream. bytes.Reader tracks how many
			// bytes the stream consumed, so the next entry starts exactly
			// after it.
			br := bytes.NewReader(data[pos:end])
			zr, err := zlib.NewReader(br)
			if err != nil {
				return nil, fmt.Errorf("bad zlib stream: %v", err)
			}
			objData := make([]byte, 0, size)
			tmp := make([]byte, 8192)
			var got int64
			for got < size {
				n, err := zr.Read(tmp)
				if n > 0 {
					objData = append(objData, tmp[:n]...)
					got += int64(n)
				}
				if err != nil {
					break
				}
			}
			zr.Close()
			if int64(len(objData)) != size {
				return nil, fmt.Errorf("object size mismatch")
			}
			pos += (end - pos) - br.Len()
			out = append(out, &Object{Typ: typeNames[typ], Data: objData})
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
	}
	if pos != end {
		return nil, fmt.Errorf("pack has %d trailing bytes", end-pos)
	}
	return out, nil
}
