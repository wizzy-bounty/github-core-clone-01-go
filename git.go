package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line framing

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

type pkt struct {
	kind    pktKind
	payload []byte // data payload (without the 4-byte length)
}

func pktLine(s string) []byte {
	b := []byte(s)
	out := make([]byte, 4+len(b))
	hexLen := fmt.Sprintf("%04x", 4+len(b))
	copy(out, hexLen)
	copy(out[4:], b)
	return out
}

func pktFlushBytes() []byte { return []byte("0000") }
func pktDelimBytes() []byte { return []byte("0001") }

// decodePktLines parses a pkt-line stream into packets.
func decodePktLines(data []byte) ([]pkt, error) {
	var out []pkt
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("truncated pkt-line header")
		}
		var n int
		if _, err := fmt.Sscanf(string(data[:4]), "%04x", &n); err != nil {
			return nil, fmt.Errorf("bad pkt-line length %q", string(data[:4]))
		}
		if n == 0 {
			out = append(out, pkt{kind: pktFlush})
			data = data[4:]
			continue
		}
		if n == 1 {
			out = append(out, pkt{kind: pktDelim})
			data = data[4:]
			continue
		}
		if n == 2 {
			out = append(out, pkt{kind: pktResponseEnd})
			data = data[4:]
			continue
		}
		if n < 4 || len(data) < n {
			return nil, fmt.Errorf("bad pkt-line length %d", n)
		}
		out = append(out, pkt{kind: pktData, payload: data[4:n]})
		data = data[n:]
	}
	return out, nil
}

func hex4(n int) string { return fmt.Sprintf("%04x", n) }

// sidebandPackets wraps data in sideband band-1 packets (max payload 65515
// data bytes per packet; the pkt-line length counts the 4 header bytes plus
// the 1 band byte).
func sidebandPackets(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		chunk := 65515
		if len(data) < chunk {
			chunk = len(data)
		}
		payload := make([]byte, 1+chunk)
		payload[0] = 1
		copy(payload[1:], data[:chunk])
		out.WriteString(hex4(4 + len(payload)))
		out.Write(payload)
		data = data[chunk:]
	}
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// Advertisements

// v2Advertisement is constant: protocol v2 advertises capabilities only.
var v2Advertisement = func() []byte {
	var b bytes.Buffer
	b.Write(pktLine("# service=git-upload-pack\n"))
	b.Write(pktFlushBytes())
	b.Write(pktLine("version 2\n"))
	b.Write(pktLine("ls-refs=unborn\n"))
	b.Write(pktLine("fetch=shallow filter\n"))
	b.Write(pktLine("object-format=sha1\n"))
	b.Write(pktLine("agent=" + serverName + "/" + serverVersion + "\n"))
	b.Write(pktFlushBytes())
	return b.Bytes()
}()

const (
	uploadPackV0Caps  = "side-band-64k thin-pack ofs-delta shallow multi_ack_detailed symref=HEAD:refs/heads/main agent=" + serverName + "/" + serverVersion
	receivePackV0Caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main agent=" + serverName + "/" + serverVersion
)

// v0Advertisement builds a v0 ref advertisement (upload-pack or
// receive-pack). First line carries the capabilities after a NUL; an empty
// repo advertises the capabilities pseudo-ref.
func v0Advertisement(r *Repo, service string, caps string) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var lines []string
	// HEAD resolves through the default branch.
	headTip, headOK := r.refs["refs/heads/"+r.defaultBranch]
	var names []string
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	first := true
	add := func(line string) {
		if first {
			line += "\x00" + caps
			first = false
		}
		lines = append(lines, line)
	}
	if headOK {
		add(headTip + " HEAD")
	} else if len(names) == 0 {
		add(zeroOID + " capabilities^{}")
	}
	for _, name := range names {
		oid := r.refs[name]
		if name == "HEAD" {
			continue
		}
		add(oid + " " + name)
		// Peeled tag line for annotated tags.
		if strings.HasPrefix(name, "refs/tags/") {
			if obj := r.objects[oid]; obj != nil && obj.Typ == "tag" {
				if tgt := parseTagTarget(obj.Data); tgt != "" {
					lines = append(lines, tgt+" "+name+"^{}")
				}
			}
		}
	}
	var b bytes.Buffer
	preamble := "# service=git-upload-pack\n"
	if service == "git-receive-pack" {
		preamble = "# service=git-receive-pack\n"
	}
	b.Write(pktLine(preamble))
	b.Write(pktFlushBytes())
	for _, line := range lines {
		b.Write(pktLine(line + "\n"))
	}
	b.Write(pktFlushBytes())
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// Git routing

// handleGit serves /{owner}/{repo}.git/info/refs, git-upload-pack and
// git-receive-pack.
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	segs := splitPath(r.URL.Path)
	if len(segs) < 3 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner := segs[0]
	repoName := strings.TrimSuffix(segs[1], ".git")
	// The first owner-bearing request adopts the owner (self-corrects the
	// derived viewer login); afterwards the owner is strict.
	s.store.adoptOwner(owner)
	repo := s.store.getRepo(owner, repoName)
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch segs[2] {
	case "info":
		if r.Method != http.MethodGet || len(segs) != 4 || segs[3] != "refs" {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.handleInfoRefs(w, r, repo)
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		s.handleUploadPack(w, r, repo)
	case "git-receive-pack":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		s.handleReceivePack(w, r, repo)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	switch service {
	case "git-upload-pack":
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if r.Header.Get("Git-Protocol") == "version=2" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(v2Advertisement)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.cachedV0Adv(repo, service))
	case "git-receive-pack":
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.cachedV0Adv(repo, service))
	default:
		writeErr(w, http.StatusBadRequest, "missing service")
	}
}

func (s *Server) cachedV0Adv(repo *Repo, service string) []byte {
	repo.mu.RLock()
	if service == "git-upload-pack" && repo.advV0Up != nil {
		b := repo.advV0Up
		repo.mu.RUnlock()
		return b
	}
	if service == "git-receive-pack" && repo.advV0Recv != nil {
		b := repo.advV0Recv
		repo.mu.RUnlock()
		return b
	}
	repo.mu.RUnlock()
	caps := uploadPackV0Caps
	if service == "git-receive-pack" {
		caps = receivePackV0Caps
	}
	b := v0Advertisement(repo, service, caps)
	repo.mu.Lock()
	if service == "git-upload-pack" {
		repo.advV0Up = b
	} else {
		repo.advV0Recv = b
	}
	repo.mu.Unlock()
	return b
}

// ---------------------------------------------------------------------------
// ls-refs (v2)

type lsRefLine struct {
	name string
	line string
}

func (s *Server) lsRefLines(repo *Repo) []lsRefLine {
	repo.mu.RLock()
	if repo.lsRefLines != nil {
		lines := repo.lsRefLines
		repo.mu.RUnlock()
		return lines
	}
	repo.mu.RUnlock()
	lines := buildLsRefLines(repo)
	repo.mu.Lock()
	repo.lsRefLines = lines
	repo.mu.Unlock()
	return lines
}

func buildLsRefLines(repo *Repo) []lsRefLine {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var lines []lsRefLine
	headTip, headOK := repo.refs["refs/heads/"+repo.defaultBranch]
	if headOK {
		lines = append(lines, lsRefLine{"HEAD", headTip + " HEAD symref-target:refs/heads/" + repo.defaultBranch})
	} else {
		// An unborn HEAD is listed only as HEAD: a refs/heads/<branch>
		// line here would read as an existing (zero-oid) ref.
		lines = append(lines, lsRefLine{"HEAD", "unborn HEAD symref-target:refs/heads/" + repo.defaultBranch})
	}
	var names []string
	for name := range repo.refs {
		if name == "HEAD" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		oid := repo.refs[name]
		line := oid + " " + name
		if strings.HasPrefix(name, "refs/tags/") {
			if obj := repo.objects[oid]; obj != nil && obj.Typ == "tag" {
				if tgt := parseTagTarget(obj.Data); tgt != "" {
					line += " peeled:" + tgt
				}
			}
		}
		lines = append(lines, lsRefLine{name, line})
	}
	return lines
}

func (s *Server) handleLsRefs(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	packets, err := decodePktLines(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad pkt-line stream")
		return
	}
	var prefixes []string
	peel := true
	for _, p := range packets {
		if p.kind != pktData {
			continue
		}
		text := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case text == "command=ls-refs":
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(text, "ref-prefix "))
		case text == "peel":
			peel = true
		}
	}
	lines := s.lsRefLines(repo)
	var b bytes.Buffer
	for _, l := range lines {
		if len(prefixes) > 0 {
			keep := false
			for _, pre := range prefixes {
				if strings.HasPrefix(l.name, pre) {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
		}
		line := l.line
		if !peel {
			// Drop the peeled attribute when the client did not ask for it.
			if idx := strings.Index(line, " peeled:"); idx >= 0 {
				line = line[:idx]
			}
		}
		b.Write(pktLine(line + "\n"))
	}
	b.Write(pktFlushBytes())
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
}

// ---------------------------------------------------------------------------
// Fetch (v2 and v0)

type fetchRequest struct {
	wants   []string
	haves   []string
	shallow []string // client's current shallow boundaries
	depth   int      // 0 = no deepen
}

func parseFetchBody(body []byte, v2 bool) (*fetchRequest, error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return nil, err
	}
	fr := &fetchRequest{}
	for _, p := range packets {
		if p.kind == pktFlush {
			continue
		}
		if p.kind != pktData {
			continue
		}
		text := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			// v0 first want carries capabilities after a space; the oid is
			// the first token.
			fr.wants = append(fr.wants, strings.Fields(text[5:])[0])
		case strings.HasPrefix(text, "have "):
			fr.haves = append(fr.haves, strings.Fields(text[5:])[0])
		case strings.HasPrefix(text, "shallow "):
			fr.shallow = append(fr.shallow, strings.Fields(text[8:])[0])
		case strings.HasPrefix(text, "deepen "):
			var d int
			fmt.Sscanf(text[7:], "%d", &d)
			fr.depth = d
		}
	}
	return fr, nil
}

// fetchObjects computes the object set and shallow-info for a fetch. held is
// the full closure of the client's haves: nothing in it is re-sent.
func (s *Server) fetchObjects(repo *Repo, fr *fetchRequest) (objs map[string]bool, shallowOut, unshallowOut []string) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	held := repo.heldClosure(fr.haves, fr.shallow)
	if fr.depth > 0 {
		objs, shallowOut, unshallowOut = repo.fetchDepth(fr.wants, fr.depth, fr.shallow, held)
		return objs, shallowOut, unshallowOut
	}
	objs = repo.reachable(fr.wants, -1, held)
	// Without a deepen, the client's boundaries stay shallow.
	for _, b := range fr.shallow {
		shallowOut = append(shallowOut, b)
	}
	return objs, shallowOut, unshallowOut
}

// fetchDepth walks wants to commit depth, returning the object set, the new
// shallow boundaries (commits at exactly depth) and the unshallowed
// boundaries (client boundaries now fully included). held objects are not
// sent, but the walk descends through them: a held tip's parents are new.
func (r *Repo) fetchDepth(wants []string, depth int, shallow []string, held map[string]bool) (map[string]bool, []string, []string) {
	out := map[string]bool{}
	visited := map[string]bool{}
	type item struct {
		oid   string
		depth int
	}
	queue := []item{}
	seen := map[string]int{} // commit oid -> shallowest depth seen
	for _, w := range wants {
		if w == "" || w == zeroOID {
			continue
		}
		queue = append(queue, item{oid: w, depth: 1})
	}
	var boundaries []string
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
			if d, ok := seen[it.oid]; !ok || it.depth < d {
				seen[it.oid] = it.depth
			}
			tree, parents := parseCommit(obj.Data)
			if tree != "" {
				queue = append(queue, item{oid: tree, depth: it.depth})
			}
			if it.depth < depth {
				for _, p := range parents {
					queue = append(queue, item{oid: p, depth: it.depth + 1})
				}
			} else {
				boundaries = append(boundaries, it.oid)
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
	// Boundaries: commits at exactly depth, deduplicated, sorted.
	bset := map[string]bool{}
	for _, b := range boundaries {
		bset[b] = true
	}
	var shallowOut []string
	for b := range bset {
		shallowOut = append(shallowOut, b)
	}
	sort.Strings(shallowOut)
	// Unshallow: a client boundary is unshallowed when it is now included
	// at a depth below the boundary (its parents are in the set).
	var unshallowOut []string
	for _, b := range shallow {
		if d, ok := seen[b]; ok && d < depth {
			unshallowOut = append(unshallowOut, b)
		}
	}
	sort.Strings(unshallowOut)
	return out, shallowOut, unshallowOut
}

// packCacheKey is the cache key for a fetch's pack bytes.
func packCacheKey(fr *fetchRequest) string {
	w := append([]string{}, fr.wants...)
	h := append([]string{}, fr.haves...)
	sh := append([]string{}, fr.shallow...)
	sort.Strings(w)
	sort.Strings(h)
	sort.Strings(sh)
	return "w:" + strings.Join(w, ",") + "|d:" + fmt.Sprint(fr.depth) + "|h:" + strings.Join(h, ",") + "|s:" + strings.Join(sh, ",")
}

// cachedPack returns the pack bytes for a fetch, building and caching them.
// The pack is built under a read lock: the object map is never mutated
// under a read lock, so the bytes are stable.
func (s *Server) cachedPack(repo *Repo, fr *fetchRequest, objs map[string]bool) []byte {
	key := packCacheKey(fr)
	repo.mu.RLock()
	if b, ok := repo.packCache[key]; ok {
		repo.mu.RUnlock()
		return b
	}
	pack := repo.buildPack(objs)
	repo.mu.RUnlock()
	repo.mu.Lock()
	if repo.packCache == nil {
		repo.packCache = map[string][]byte{}
	}
	repo.packCache[key] = pack
	repo.mu.Unlock()
	return pack
}

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read body")
		return
	}
	v2 := r.Header.Get("Git-Protocol") == "version=2"
	// The command decides the operation: ls-refs vs fetch.
	isLsRefs := bytes.Contains(body, []byte("command=ls-refs"))
	if v2 && isLsRefs {
		s.handleLsRefs(w, r, repo, body)
		return
	}
	fr, err := parseFetchBody(body, v2)
	if err != nil {
		// Malformed pkt-line stream: answer 400, stay alive.
		writeErr(w, http.StatusBadRequest, "bad pkt-line stream")
		return
	}
	objs, shallowOut, unshallowOut := s.fetchObjects(repo, fr)
	var pack []byte
	if len(objs) > 0 {
		pack = s.cachedPack(repo, fr, objs)
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(http.StatusOK)
	if v2 {
		var b bytes.Buffer
		b.Write(pktLine("acknowledgments\n"))
		b.Write(pktLine("NAK\n"))
		b.Write(pktDelimBytes())
		if len(shallowOut) > 0 || len(unshallowOut) > 0 {
			b.Write(pktLine("shallow-info\n"))
			for _, o := range shallowOut {
				b.Write(pktLine("shallow " + o + "\n"))
			}
			for _, o := range unshallowOut {
				b.Write(pktLine("unshallow " + o + "\n"))
			}
			b.Write(pktDelimBytes())
		}
		if pack != nil {
			b.Write(pktLine("packfile\n"))
			b.Write(sidebandPackets(pack))
		}
		b.Write(pktFlushBytes())
		_, _ = w.Write(b.Bytes())
		return
	}
	// v0: NAK, shallow lines, then the sideband pack.
	var b bytes.Buffer
	b.Write(pktLine("NAK\n"))
	for _, o := range shallowOut {
		b.Write(pktLine("shallow " + o + "\n"))
	}
	for _, o := range unshallowOut {
		b.Write(pktLine("unshallow " + o + "\n"))
	}
	if pack != nil {
		b.Write(sidebandPackets(pack))
	}
	b.Write(pktFlushBytes())
	_, _ = w.Write(b.Bytes())
}

// ---------------------------------------------------------------------------
// Receive-pack (push)

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read body")
		return
	}
	// Parse the command list pkt-line by pkt-line, stopping at the first
	// flush: everything after it is the raw pack, which must NOT be parsed
	// as pkt-lines.
	var commands []pushCommand
	pos := 0
	seenFlush := false
	for pos+4 <= len(body) {
		var n int
		if _, err := fmt.Sscanf(string(body[pos:pos+4]), "%04x", &n); err != nil {
			writeErr(w, http.StatusBadRequest, "bad pkt-line stream")
			return
		}
		if n == 0 {
			pos += 4
			seenFlush = true
			break
		}
		if n < 4 || pos+n > len(body) {
			writeErr(w, http.StatusBadRequest, "bad pkt-line stream")
			return
		}
		text := strings.TrimSuffix(string(body[pos+4:pos+n]), "\n")
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul] // capabilities on the first command line
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			writeErr(w, http.StatusBadRequest, "bad push command")
			return
		}
		commands = append(commands, pushCommand{oldOid: fields[0], newOid: fields[1], ref: fields[2]})
		pos += n
	}
	if !seenFlush {
		writeErr(w, http.StatusBadRequest, "push command list has no flush")
		return
	}
	if len(commands) == 0 {
		writeErr(w, http.StatusBadRequest, "push has no commands")
		return
	}
	packStart := pos
	// Unpack the pack (if any bytes follow the command list).
	var packObjs []*Object
	unpackErr := ""
	if packStart < len(body) {
		packObjs, err = parsePack(body[packStart:])
		if err != nil {
			unpackErr = err.Error()
		}
	}
	// Report helper: pkt-lined lines inside sideband band-1 packets.
	report := func(w http.ResponseWriter, lines []string) {
		var inner bytes.Buffer
		for _, line := range lines {
			inner.Write(pktLine(line + "\n"))
		}
		var out bytes.Buffer
		out.Write(sidebandPackets(inner.Bytes()))
		out.Write(pktFlushBytes())
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out.Bytes())
	}
	if unpackErr != "" {
		var lines []string
		lines = append(lines, "unpack "+unpackErr)
		for _, c := range commands {
			lines = append(lines, "ng "+c.ref+" unpack failed")
		}
		report(w, lines)
		return
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, o := range packObjs {
		repo.putObject(o.Typ, o.Data)
	}
	var lines []string
	lines = append(lines, "unpack ok")
	changed := false
	for _, c := range commands {
		if !strings.HasPrefix(c.ref, "refs/") {
			lines = append(lines, "ng "+c.ref+" invalid ref")
			continue
		}
		current, exists := repo.refs[c.ref]
		if c.newOid == zeroOID {
			// Delete.
			if !exists {
				lines = append(lines, "ng "+c.ref+" ref does not exist")
				continue
			}
			if c.oldOid != current {
				lines = append(lines, "ng "+c.ref+" non-fast-forward")
				continue
			}
			delete(repo.refs, c.ref)
			changed = true
			lines = append(lines, "ok "+c.ref)
			continue
		}
		// Create or update: the new object must exist.
		newObj := repo.objects[c.newOid]
		if newObj == nil {
			lines = append(lines, "ng "+c.ref+" object not found")
			continue
		}
		if c.oldOid == zeroOID {
			if exists {
				lines = append(lines, "ng "+c.ref+" already exists")
				continue
			}
			repo.refs[c.ref] = c.newOid
			changed = true
			lines = append(lines, "ok "+c.ref)
			continue
		}
		if !exists || c.oldOid != current {
			lines = append(lines, "ng "+c.ref+" non-fast-forward")
			continue
		}
		if !repo.isAncestor(c.oldOid, c.newOid) {
			lines = append(lines, "ng "+c.ref+" non-fast-forward")
			continue
		}
		repo.refs[c.ref] = c.newOid
		changed = true
		lines = append(lines, "ok "+c.ref)
	}
	if changed {
		repo.bump()
	}
	report(w, lines)
}
