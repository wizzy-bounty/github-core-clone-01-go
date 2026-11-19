package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// /api/v3 root, /user, /user/repos

func (s *Server) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo, read:org")
	writeJSON(w, http.StatusOK, map[string]any{
		"current_user_url": s.baseURL(r) + "/api/v3/user",
		"repository_url":   s.baseURL(r) + "/api/v3/repos/{owner}/{repo}",
	})
}

func (s *Server) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"login": s.store.viewer(),
		"id":    1000,
		"type":  "User",
	})
}

// handleUserRepos serves GET (paginated list) and POST (create) /user/repos.
func (s *Server) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUserRepos(w, r)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		if body.Name == "" {
			writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		repo, ok := s.store.createRepo(body.Name, body.Description, body.Private)
		if !ok {
			writeErr(w, http.StatusUnprocessableEntity, "name already exists on this account")
			return
		}
		writeJSON(w, http.StatusCreated, s.repoObject(r, repo))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *Server) listUserRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := 30
	if v := q.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	repos := s.store.listRepos()
	total := len(repos)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	// Link header: rel="next" (and rel="last") while more pages remain.
	if end < total {
		next := fmt.Sprintf("<%s/api/v3/user/repos?per_page=%d&page=%d>; rel=\"next\"", s.baseURL(r), perPage, page+1)
		last := (total + perPage - 1) / perPage
		next += fmt.Sprintf(", <%s/api/v3/user/repos?per_page=%d&page=%d>; rel=\"last\"", s.baseURL(r), perPage, last)
		w.Header().Set("Link", next)
	}
	items := make([]any, 0, end-start)
	for _, repo := range repos[start:end] {
		items = append(items, s.repoObject(r, repo))
	}
	writeJSON(w, http.StatusOK, items)
}

// repoObject renders a repo as a REST object.
func (s *Server) repoObject(r *http.Request, repo *Repo) map[string]any {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	viewer := s.store.viewer()
	return map[string]any{
		"id":             repo.id,
		"name":           repo.name,
		"full_name":      viewer + "/" + repo.name,
		"owner":          map[string]any{"login": viewer, "id": 1000, "type": "User"},
		"private":        repo.priv,
		"description":    repo.desc,
		"default_branch": repo.defaultBranch,
		"html_url":       s.baseURL(r) + "/" + viewer + "/" + repo.name,
		"clone_url":      s.baseURL(r) + "/" + viewer + "/" + repo.name + ".git",
		"url":            s.baseURL(r) + "/api/v3/repos/" + viewer + "/" + repo.name,
		"created_at":     isoTime(scenarioEpochSec),
		"updated_at":     isoTime(scenarioEpochSec),
	}
}

// ---------------------------------------------------------------------------
// /api/v3/repos/{owner}/{repo}/...

func (s *Server) handleRepoAPI(w http.ResponseWriter, r *http.Request) {
	segs := splitPath(r.URL.Path) // api, v3, repos, owner, name, ...
	if len(segs) < 5 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := segs[3], segs[4]
	s.store.adoptOwner(owner)
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	rest := segs[5:]
	if len(rest) == 0 {
		s.handleRepoSelf(w, r, repo)
		return
	}
	switch rest[0] {
	case "branches":
		s.handleBranches(w, r, repo, rest[1:])
	case "commits":
		s.handleCommits(w, r, repo)
	case "git":
		s.handleGitRefs(w, r, repo, rest[1:])
	case "contents":
		s.handleContents(w, r, repo, rest[1:])
	case "labels":
		s.handleLabels(w, r, repo)
	case "issues":
		s.handleIssues(w, r, repo, rest[1:])
	case "pulls":
		s.handlePulls(w, r, repo, rest[1:])
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleRepoSelf(w http.ResponseWriter, r *http.Request, repo *Repo) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.repoObject(r, repo))
	case http.MethodPatch:
		var body struct {
			Description *string `json:"description"`
			Private     *bool   `json:"private"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		repo.mu.Lock()
		if body.Description != nil {
			repo.desc = *body.Description
		}
		if body.Private != nil {
			repo.priv = *body.Private
		}
		repo.mu.Unlock()
		writeJSON(w, http.StatusOK, s.repoObject(r, repo))
	case http.MethodDelete:
		s.store.deleteRepo(repo.name)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

// ---------------------------------------------------------------------------
// Branches, commits, git refs

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if len(rest) == 0 {
		var names []string
		for ref := range repo.refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		sort.Strings(names)
		items := make([]any, 0, len(names))
		for _, n := range names {
			items = append(items, map[string]any{
				"name":   n,
				"commit": map[string]any{"sha": repo.refs["refs/heads/"+n]},
			})
		}
		writeJSON(w, http.StatusOK, items)
		return
	}
	branch := rest[0]
	oid, ok := repo.refs["refs/heads/"+branch]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   branch,
		"commit": map[string]any{"sha": oid},
	})
}

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	sha := r.URL.Query().Get("sha")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var tip string
	if sha == "" {
		tip, _ = repo.refs["refs/heads/"+repo.defaultBranch]
	} else if strings.HasPrefix(sha, "refs/") {
		tip = repo.refs[sha]
	} else if oid, ok := repo.refs["refs/heads/"+sha]; ok {
		tip = oid
	} else {
		tip = sha // a raw commit oid
	}
	var items []any
	for _, oid := range repo.commitHistory(tip) {
		obj := repo.objects[oid]
		if obj == nil {
			continue
		}
		items = append(items, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				"message": commitMessage(obj.Data),
			},
		})
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGitRefs(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) >= 1 && rest[0] == "refs" {
		if r.Method == http.MethodPost && len(rest) == 1 {
			var body struct {
				Ref string `json:"ref"`
				Sha string `json:"sha"`
			}
			if err := decodeJSONBody(r, &body); err != nil {
				writeErr(w, http.StatusBadRequest, "malformed JSON body")
				return
			}
			if !strings.HasPrefix(body.Ref, "refs/") || body.Sha == "" {
				writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
				return
			}
			repo.mu.Lock()
			if _, ok := repo.objects[body.Sha]; !ok {
				repo.mu.Unlock()
				writeErr(w, http.StatusUnprocessableEntity, "sha does not exist")
				return
			}
			if _, exists := repo.refs[body.Ref]; exists {
				repo.mu.Unlock()
				writeErr(w, http.StatusUnprocessableEntity, "ref already exists")
				return
			}
			repo.refs[body.Ref] = body.Sha
			repo.bump()
			repo.mu.Unlock()
			writeJSON(w, http.StatusCreated, map[string]any{
				"ref":    body.Ref,
				"object": map[string]any{"sha": body.Sha},
			})
			return
		}
		if r.Method == http.MethodGet && len(rest) == 3 && rest[1] == "heads" {
			branch := rest[2]
			repo.mu.RLock()
			oid, ok := repo.refs["refs/heads/"+branch]
			repo.mu.RUnlock()
			if !ok {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ref":    "refs/heads/" + branch,
				"object": map[string]any{"sha": oid},
			})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// ---------------------------------------------------------------------------
// Contents

func (s *Server) handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	path := strings.Join(rest, "/")
	branch := r.URL.Query().Get("ref")
	if branch == "" {
		branch = repo.defaultBranch
	}
	switch r.Method {
	case http.MethodGet:
		s.getContent(w, r, repo, path, branch)
	case http.MethodPut:
		s.putContent(w, r, repo, path, branch)
	case http.MethodDelete:
		s.deleteContent(w, r, repo, path, branch)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request, repo *Repo, path, branch string) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	tip, ok := repo.refs["refs/heads/"+branch]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	obj := repo.objects[tip]
	if obj == nil || obj.Typ != "commit" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	tree, _ := parseCommit(obj.Data)
	segments := strings.Split(path, "/")
	oid, isDir, found := repo.treeLookup(tree, segments)
	if !found {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if isDir {
		dirObj := repo.objects[oid]
		if dirObj == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		var items []any
		for _, e := range parseTree(dirObj.Data) {
			typ := "file"
			if e.mode == "40000" {
				typ = "dir"
			}
			items = append(items, map[string]any{
				"type": typ,
				"name": e.name,
				"path": path + "/" + e.name,
				"sha":  e.oid,
			})
		}
		if items == nil {
			items = []any{}
		}
		writeJSON(w, http.StatusOK, items)
		return
	}
	blob := repo.objects[oid]
	if blob == nil || blob.Typ != "blob" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(blob.Data),
		"sha":      oid,
		"size":     len(blob.Data),
		"name":     segments[len(segments)-1],
		"path":     path,
	})
}

func (s *Server) putContent(w http.ResponseWriter, r *http.Request, repo *Repo, path, branch string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Sha     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if body.Branch != "" {
		branch = body.Branch
	}
	raw, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "content is not valid base64")
		return
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	tip, hasTip := repo.refs["refs/heads/"+branch]
	var tree string
	var parents []string
	if hasTip {
		if obj := repo.objects[tip]; obj != nil && obj.Typ == "commit" {
			tree, _ = parseCommit(obj.Data)
			parents = []string{tip}
		}
	}
	segments := strings.Split(path, "/")
	var currentSha string
	if tree != "" {
		if oid, isDir, found := repo.treeLookup(tree, segments); found && !isDir {
			currentSha = oid
		}
	}
	isUpdate := currentSha != ""
	if isUpdate {
		if body.Sha == "" {
			writeErr(w, http.StatusUnprocessableEntity, "sha is required to update a file")
			return
		}
		if body.Sha != currentSha {
			writeErr(w, http.StatusConflict, "sha does not match the current file")
			return
		}
	} else if body.Sha != "" {
		writeErr(w, http.StatusUnprocessableEntity, "sha does not match a new file")
		return
	}
	blobOid := repo.putObject("blob", raw)
	newTree := repo.treeWithPath(tree, segments, blobOid)
	// The commit message is exactly the submitted message.
	commitOid := repo.putObject("commit", buildCommit(newTree, parents, body.Message, s.store.nextTime()))
	repo.refs["refs/heads/"+branch] = commitOid
	repo.bump()
	status := http.StatusCreated
	if isUpdate {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"name":     segments[len(segments)-1],
			"path":     path,
			"sha":      blobOid,
			"size":     len(raw),
			"encoding": "base64",
		},
		"commit": map[string]any{"sha": commitOid},
	})
}

func (s *Server) deleteContent(w http.ResponseWriter, r *http.Request, repo *Repo, path, branch string) {
	var body struct {
		Message string `json:"message"`
		Sha     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if body.Branch != "" {
		branch = body.Branch
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	tip, hasTip := repo.refs["refs/heads/"+branch]
	if !hasTip {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	obj := repo.objects[tip]
	if obj == nil || obj.Typ != "commit" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	tree, _ := parseCommit(obj.Data)
	segments := strings.Split(path, "/")
	oid, isDir, found := repo.treeLookup(tree, segments)
	if !found || isDir {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Sha != "" && body.Sha != oid {
		writeErr(w, http.StatusConflict, "sha does not match the current file")
		return
	}
	newTree := repo.treeDeletePath(tree, segments)
	if newTree == "" {
		// An empty tree object: store the canonical empty tree.
		newTree = repo.putObject("tree", []byte{})
	}
	commitOid := repo.putObject("commit", buildCommit(newTree, []string{tip}, body.Message, s.store.nextTime()))
	repo.refs["refs/heads/"+branch] = commitOid
	repo.bump()
	writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]any{"sha": commitOid},
	})
}

// ---------------------------------------------------------------------------
// Labels

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request, repo *Repo) {
	switch r.Method {
	case http.MethodGet:
		repo.mu.RLock()
		var items []any
		for _, name := range repo.labelOrder {
			l := repo.labels[name]
			items = append(items, map[string]any{"name": l.Name, "color": l.Color})
		}
		repo.mu.RUnlock()
		if items == nil {
			items = []any{}
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		if body.Name == "" {
			writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		repo.mu.Lock()
		if _, exists := repo.labels[body.Name]; !exists {
			repo.labelOrder = append(repo.labelOrder, body.Name)
		}
		if body.Color == "" {
			body.Color = "ededed"
		}
		repo.labels[body.Name] = &Label{Name: body.Name, Color: body.Color}
		repo.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"name": body.Name, "color": body.Color})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

// ---------------------------------------------------------------------------
// Issues

// issueObject renders an issue. The body is cached at creation/update time;
// this builder is only used then.
func (s *Server) issueObject(repo *Repo, iss *Issue) []byte {
	var labels []any
	for _, name := range iss.Labels {
		color := "ededed"
		if l, ok := repo.labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	if labels == nil {
		labels = []any{}
	}
	v := map[string]any{
		"id":         iss.DBID,
		"number":     iss.Number,
		"title":      iss.Title,
		"body":       iss.Body,
		"state":      iss.State,
		"labels":     labels,
		"user":       map[string]any{"login": s.store.viewer()},
		"created_at": isoTime(iss.CreatedAt),
		"updated_at": isoTime(iss.CreatedAt),
	}
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listIssues(w, r, repo)
		case http.MethodPost:
			s.createIssue(w, r, repo)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			s.listComments(w, r, repo, num)
		case http.MethodPost:
			s.createComment(w, r, repo, num)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	if len(rest) != 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Hot path: serve the precomputed body, O(1).
		repo.mu.RLock()
		b, ok := repo.issueJSON[num]
		repo.mu.RUnlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeRawJSON(w, http.StatusOK, b)
	case http.MethodPatch:
		var body struct {
			State *string `json:"state"`
			Title *string `json:"title"`
			Body  *string `json:"body"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		repo.mu.Lock()
		iss, ok := repo.issueByNum[num]
		if !ok {
			repo.mu.Unlock()
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if body.State != nil {
			iss.State = *body.State
		}
		if body.Title != nil {
			iss.Title = *body.Title
		}
		if body.Body != nil {
			iss.Body = *body.Body
		}
		b := s.issueObject(repo, iss)
		iss.cached = b
		repo.issueJSON[num] = b
		repo.mu.Unlock()
		writeRawJSON(w, http.StatusOK, b)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortOrder := q.Get("sort")
	if sortOrder == "" {
		sortOrder = "created"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := 30
	if v := q.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	repo.mu.RLock()
	var list []*Issue
	for _, iss := range repo.issues {
		if state != "all" && iss.State != state {
			continue
		}
		list = append(list, iss)
	}
	repo.mu.RUnlock()
	// Creation order is number order; sort by it.
	if direction == "desc" {
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	} else {
		sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	}
	_ = sortOrder // only "created" is supported; it is number order
	start := (page - 1) * perPage
	if start > len(list) {
		start = len(list)
	}
	end := start + perPage
	if end > len(list) {
		end = len(list)
	}
	var buf strings.Builder
	buf.WriteByte('[')
	for i, iss := range list[start:end] {
		if i > 0 {
			buf.WriteByte(',')
		}
		repo.mu.RLock()
		b := repo.issueJSON[iss.Number]
		repo.mu.RUnlock()
		buf.Write(b)
	}
	buf.WriteByte(']')
	writeRawJSON(w, http.StatusOK, []byte(buf.String()))
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	iss, errMsg := s.createIssueInRepo(repo, body.Title, body.Body, body.Labels)
	if errMsg != "" {
		writeErr(w, http.StatusUnprocessableEntity, errMsg)
		return
	}
	writeRawJSON(w, http.StatusCreated, iss.cached)
}

// createIssueInRepo creates an issue and returns it (with its cached body).
// errMsg is non-empty on failure (e.g. an unknown label).
func (s *Server) createIssueInRepo(repo *Repo, title, body string, labels []string) (*Issue, string) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, name := range labels {
		if _, ok := repo.labels[name]; !ok {
			return nil, "unknown label " + name
		}
	}
	s.store.mu.Lock()
	dbID := s.store.nextIssueDB
	s.store.nextIssueDB++
	s.store.mu.Unlock()
	iss := &Issue{
		Number:    repo.nextIssue,
		DBID:      dbID,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labels...),
		CreatedAt: s.store.nextTime(),
	}
	repo.nextIssue++
	repo.issues = append(repo.issues, iss)
	repo.issueByNum[iss.Number] = iss
	b := s.issueObject(repo, iss)
	iss.cached = b
	repo.issueJSON[iss.Number] = b
	return iss, ""
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, repo *Repo, num int) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if _, ok := repo.issueByNum[num]; !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	var items []any
	for _, c := range repo.comments[num] {
		items = append(items, map[string]any{
			"id":         c.ID,
			"body":       c.Body,
			"created_at": isoTime(c.CreatedAt),
			"user":       map[string]any{"login": s.store.viewer()},
		})
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request, repo *Repo, num int) {
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	c, errMsg := s.addCommentInRepo(repo, num, body.Body)
	if errMsg != "" {
		writeErr(w, http.StatusNotFound, errMsg)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"created_at": isoTime(c.CreatedAt),
		"user":       map[string]any{"login": s.store.viewer()},
	})
}

// addCommentInRepo appends a comment to an issue. errMsg is non-empty when
// the issue does not exist.
func (s *Server) addCommentInRepo(repo *Repo, num int, body string) (*Comment, string) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if _, ok := repo.issueByNum[num]; !ok {
		return nil, "Not Found"
	}
	c := &Comment{
		ID:        repo.nextComment,
		Body:      body,
		IssueNum:  num,
		CreatedAt: s.store.nextTime(),
	}
	repo.nextComment++
	repo.comments[num] = append(repo.comments[num], c)
	return c, ""
}

// ---------------------------------------------------------------------------
// Pull requests

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listPulls(w, r, repo)
		case http.MethodPost:
			s.createPull(w, r, repo)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		s.pullFiles(w, r, repo, num)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPut {
		s.mergePull(w, r, repo, num)
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		repo.mu.RLock()
		pr, ok := repo.pullByNum[num]
		repo.mu.RUnlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, s.pullObject(repo, pr))
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) pullObject(repo *Repo, pr *Pull) map[string]any {
	v := map[string]any{
		"id":     pr.DBID,
		"number": pr.Number,
		"title":  pr.Title,
		"body":   pr.Body,
		"state":  pr.State,
		"head": map[string]any{
			"ref": pr.HeadRef,
			"sha": pr.HeadSha,
		},
		"base": map[string]any{
			"ref": pr.BaseRef,
			"sha": pr.BaseSha,
		},
		"merged":          pr.Merged,
		"merge_commit_sha": pr.MergeSha,
		"created_at":      isoTime(pr.CreatedAt),
		"updated_at":      isoTime(pr.CreatedAt),
	}
	if pr.Merged {
		v["merged_at"] = pr.MergedAt
	}
	return v
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var items []any
	for _, pr := range repo.pulls {
		if state != "all" && pr.State != state {
			continue
		}
		items = append(items, s.pullObject(repo, pr))
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if body.Base == "" {
		body.Base = repo.defaultBranch
	}
	pr, errMsg := s.createPullInRepo(repo, body.Title, body.Body, body.Head, body.Base)
	if errMsg != "" {
		writeErr(w, http.StatusUnprocessableEntity, errMsg)
		return
	}
	writeJSON(w, http.StatusCreated, s.pullObject(repo, pr))
}

// createPullInRepo creates a pull request. errMsg is non-empty on failure.
func (s *Server) createPullInRepo(repo *Repo, title, body, head, base string) (*Pull, string) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	headSha, ok := repo.refs["refs/heads/"+head]
	if !ok {
		return nil, "head branch does not exist"
	}
	baseSha, ok := repo.refs["refs/heads/"+base]
	if !ok {
		return nil, "base branch does not exist"
	}
	for _, pr := range repo.pulls {
		if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
			return nil, "a pull request from " + head + " is already open"
		}
	}
	s.store.mu.Lock()
	dbID := s.store.nextPullDB
	s.store.nextPullDB++
	s.store.mu.Unlock()
	pr := &Pull{
		Number:        repo.nextPull,
		DBID:          dbID,
		Title:         title,
		Body:          body,
		State:         "open",
		HeadRef:       head,
		BaseRef:       base,
		HeadSha:       headSha,
		BaseSha:       baseSha,
		BaseTipCreate: baseSha,
		CreatedAt:     s.store.nextTime(),
	}
	repo.nextPull++
	repo.pulls = append(repo.pulls, pr)
	repo.pullByNum[pr.Number] = pr
	return pr, ""
}

// diffFiles returns the file diff between two trees: filename -> change.
type fileChange struct {
	status    string // "added" | "modified" | "removed"
	additions int
	deletions int
}

func (s *Server) diffFiles(repo *Repo, baseTree, headTree string) map[string]*fileChange {
	base := repo.flattenTree(baseTree)
	head := repo.flattenTree(headTree)
	out := map[string]*fileChange{}
	countLines := func(oid string) int {
		obj := repo.objects[oid]
		if obj == nil {
			return 0
		}
		n := 0
		for _, b := range obj.Data {
			if b == '\n' {
				n++
			}
		}
		if len(obj.Data) > 0 && obj.Data[len(obj.Data)-1] != '\n' {
			n++
		}
		return n
	}
	for path, hoid := range head {
		if boid, ok := base[path]; !ok {
			out[path] = &fileChange{status: "added", additions: countLines(hoid)}
		} else if boid != hoid {
			out[path] = &fileChange{status: "modified", additions: countLines(hoid), deletions: countLines(boid)}
		}
	}
	for path, boid := range base {
		if _, ok := head[path]; !ok {
			out[path] = &fileChange{status: "removed", deletions: countLines(boid)}
		}
	}
	return out
}

func (s *Server) pullFiles(w http.ResponseWriter, r *http.Request, repo *Repo, num int) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	pr, ok := repo.pullByNum[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	// Diff the base tip at creation against the head tip.
	baseObj := repo.objects[pr.BaseSha]
	headObj := repo.objects[pr.HeadSha]
	if baseObj == nil || headObj == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	baseTree, _ := parseCommit(baseObj.Data)
	headTree, _ := parseCommit(headObj.Data)
	diff := s.diffFiles(repo, baseTree, headTree)
	var names []string
	for n := range diff {
		names = append(names, n)
	}
	sort.Strings(names)
	var items []any
	for _, n := range names {
		c := diff[n]
		items = append(items, map[string]any{
			"filename":  n,
			"status":    c.status,
			"additions": c.additions,
			"deletions": c.deletions,
			"changes":   c.additions + c.deletions,
		})
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) mergePull(w http.ResponseWriter, r *http.Request, repo *Repo, num int) {
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	pr, ok := repo.pullByNum[num]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	mergeOid, msg, status, errMsg := s.mergePullInternal(repo, pr)
	if errMsg != "" {
		writeErr(w, status, errMsg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"merged":  true,
		"sha":     mergeOid,
		"message": msg,
	})
}

// mergePullInternal squash-merges an open pull request. The caller must hold
// repo.mu (write). Returns (mergeOid, message, httpStatus, errMsg); errMsg
// is non-empty on failure, with status 405 for a conflict.
func (s *Server) mergePullInternal(repo *Repo, pr *Pull) (string, string, int, string) {
	if pr.State != "open" {
		return "", "", http.StatusMethodNotAllowed, "pull request is not open"
	}
	baseTip, ok := repo.refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return "", "", http.StatusNotFound, "Not Found"
	}
	headTip, ok := repo.refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return "", "", http.StatusNotFound, "Not Found"
	}
	if s.pullConflict(repo, pr, baseTip, headTip) {
		return "", "", http.StatusMethodNotAllowed, "merge conflict"
	}
	baseObj := repo.objects[pr.BaseSha]
	headObj := repo.objects[headTip]
	curBaseObj := repo.objects[baseTip]
	if baseObj == nil || headObj == nil || curBaseObj == nil {
		return "", "", http.StatusNotFound, "Not Found"
	}
	baseTree, _ := parseCommit(baseObj.Data)
	headTree, _ := parseCommit(headObj.Data)
	curBaseTree, _ := parseCommit(curBaseObj.Data)
	// Squash merge: tree = base tree with the head's changes applied.
	mergedFiles := repo.flattenTree(curBaseTree)
	headFiles := repo.flattenTree(headTree)
	for path, oid := range headFiles {
		mergedFiles[path] = oid
	}
	for path := range s.diffFiles(repo, baseTree, headTree) {
		if _, ok := headFiles[path]; !ok {
			delete(mergedFiles, path)
		}
	}
	mergedTree := s.storeTreeFromFiles(repo, mergedFiles)
	ts := s.store.nextTime()
	msg := fmt.Sprintf("Merge pull request #%d from %s/%s", pr.Number, s.store.viewer(), pr.HeadRef)
	mergeOid := repo.putObject("commit", buildCommit(mergedTree, []string{baseTip}, msg, ts))
	repo.refs["refs/heads/"+pr.BaseRef] = mergeOid
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = isoTime(ts)
	pr.MergeSha = mergeOid
	repo.bump()
	return mergeOid, msg, http.StatusOK, ""
}

// pullConflict reports whether merging pr now would conflict: a file
// changed on the base since the PR's base tip AND changed on the head vs the
// PR's base tip, with different content. The caller must hold repo.mu.
func (s *Server) pullConflict(repo *Repo, pr *Pull, baseTip, headTip string) bool {
	baseObj := repo.objects[pr.BaseSha]
	headObj := repo.objects[headTip]
	curBaseObj := repo.objects[baseTip]
	if baseObj == nil || headObj == nil || curBaseObj == nil {
		return false
	}
	baseTree, _ := parseCommit(baseObj.Data)
	headTree, _ := parseCommit(headObj.Data)
	curBaseTree, _ := parseCommit(curBaseObj.Data)
	headDiff := s.diffFiles(repo, baseTree, headTree)
	baseDiff := s.diffFiles(repo, baseTree, curBaseTree)
	headFiles := repo.flattenTree(headTree)
	curBaseFiles := repo.flattenTree(curBaseTree)
	for path := range headDiff {
		if _, changedOnBase := baseDiff[path]; changedOnBase {
			if headFiles[path] != curBaseFiles[path] {
				return true
			}
		}
	}
	return false
}

// storeTreeFromFiles builds a tree from a flat path -> blob map.
func (s *Server) storeTreeFromFiles(repo *Repo, files map[string]string) string {
	type node struct {
		entries map[string]*node
		blob    string // non-empty for a file
	}
	root := &node{entries: map[string]*node{}}
	for path, oid := range files {
		parts := strings.Split(path, "/")
		cur := root
		for i, part := range parts {
			child, ok := cur.entries[part]
			if !ok {
				child = &node{entries: map[string]*node{}}
				cur.entries[part] = child
			}
			cur = child
			if i == len(parts)-1 {
				cur.blob = oid
			}
		}
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		names := make([]string, 0, len(n.entries))
		for name := range n.entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := n.entries[name]
			if child.blob != "" {
				entries = append(entries, treeEntry{mode: "100644", name: name, oid: child.blob})
			} else {
				entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(child)})
			}
		}
		return repo.storeTree(entries)
	}
	return build(root)
}
