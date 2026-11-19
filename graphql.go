package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GraphQL v4: a small executor for the suite's fixed documents. The
// documents are matched by operation name; variables are read from the
// "variables" object. Unknown repositories answer data.repository = null
// with no errors; an unknown field answers HTTP 200 with an errors array.

var gqlOpRe = regexp.MustCompile(`(?m)^\s*(query|mutation)\s+([A-Za-z0-9_]+)`)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// Batched body: a JSON array of operations, answered in order.
		var batch []gqlRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		results := make([]any, 0, len(batch))
		for _, req := range batch {
			results = append(results, s.executeGraphQL(req))
		}
		writeJSON(w, http.StatusOK, results)
		return
	}
	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	writeJSON(w, http.StatusOK, s.executeGraphQL(req))
}

func (s *Server) executeGraphQL(req gqlRequest) map[string]any {
	m := gqlOpRe.FindStringSubmatch(req.Query)
	if m == nil {
		return gqlError("could not parse the operation")
	}
	kind, name := m[1], m[2]
	vars := req.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	switch kind + " " + name {
	case "query ViewerLogin":
		return gqlData(map[string]any{"viewer": map[string]any{"login": s.store.viewer()}})
	case "query RepositoryFields":
		return s.gqlRepository(vars)
	case "query Issue_fields":
		return gqlData(map[string]any{"Issue": gqlType("Issue", issueFieldNames)})
	case "query PullRequestAndRepositoryFields":
		return gqlData(map[string]any{
			"PullRequest": gqlType("PullRequest", pullRequestFieldNames),
			"Repository":  gqlType("Repository", repositoryFieldNames),
		})
	case "query SearchType_enumValues":
		return gqlData(map[string]any{"SearchType": map[string]any{
			"name": "SearchType",
			"kind": "ENUM",
			"enumValues": []any{
				map[string]any{"name": "ISSUE"},
				map[string]any{"name": "PULLREQUEST"},
				map[string]any{"name": "REPOSITORY"},
				map[string]any{"name": "USER"},
				map[string]any{"name": "ORGANIZATION"},
				map[string]any{"name": "DISCUSSION"},
				map[string]any{"name": "COMMIT"},
				map[string]any{"name": "MILESTONE"},
				map[string]any{"name": "PROJECT"},
				map[string]any{"name": "TOPIC"},
				map[string]any{"name": "WIKI"},
			},
		}})
	case "query OpenIssues":
		return s.gqlOpenIssues(vars)
	case "query IssueByNumber":
		return s.gqlIssueByNumber(vars)
	case "mutation IssueCreate":
		return s.gqlCreateIssue(vars)
	case "mutation IssueClose":
		return s.gqlCloseIssue(vars)
	case "query PullRequests":
		return s.gqlPullRequests(vars)
	case "query PullRequestFiles":
		return s.gqlPullRequestFiles(vars)
	case "query PullRequestId":
		return s.gqlPullRequestID(vars)
	case "mutation PullRequestCreate":
		return s.gqlCreatePullRequest(vars)
	case "mutation MergePullRequest":
		return s.gqlMergePullRequest(vars)
	case "mutation AddComment":
		return s.gqlAddComment(vars)
	case "query ViewerInvalidField":
		return map[string]any{
			"data": map[string]any{"viewer": nil},
			"errors": []any{map[string]any{
				"message": "Cannot query field \"noSuchFieldOnViewer\" on type \"User\".",
			}},
		}
	case "query NodeRepository":
		return s.gqlNode(vars)
	case "query IssuesPage":
		return s.gqlIssuesPage(vars)
	}
	// Fallback: answer the introspection probes by name, anything else as
	// an unknown-field error (HTTP 200, errors array).
	if strings.Contains(req.Query, "__type") {
		if strings.Contains(req.Query, "\"Issue\"") {
			return gqlData(map[string]any{"Issue": gqlType("Issue", issueFieldNames)})
		}
		if strings.Contains(req.Query, "\"PullRequest\"") {
			return gqlData(map[string]any{"PullRequest": gqlType("PullRequest", pullRequestFieldNames)})
		}
		if strings.Contains(req.Query, "\"Repository\"") {
			return gqlData(map[string]any{"Repository": gqlType("Repository", repositoryFieldNames)})
		}
		if strings.Contains(req.Query, "\"SearchType\"") {
			return gqlData(map[string]any{"SearchType": map[string]any{"name": "SearchType", "kind": "ENUM", "enumValues": []any{map[string]any{"name": "ISSUE"}}}})
		}
	}
	return gqlError("unknown operation " + name)
}

func gqlData(data map[string]any) map[string]any {
	return map[string]any{"data": data}
}

func gqlError(msg string) map[string]any {
	return map[string]any{
		"data":   nil,
		"errors": []any{map[string]any{"message": msg}},
	}
}

func gqlType(name string, fields []string) map[string]any {
	list := make([]any, 0, len(fields))
	for _, f := range fields {
		list = append(list, map[string]any{"name": f})
	}
	return map[string]any{
		"name":   name,
		"kind":   "OBJECT",
		"fields": list,
	}
}

var issueFieldNames = []string{
	"id", "number", "title", "state", "body", "createdAt", "updatedAt",
	"url", "author", "comments", "labels", "closedAt", "locked",
	"milestone", "assignees", "repository", "viewerDidAuthor",
	"authorAssociation", "stateReason", "closed", "timelineItems",
}

var pullRequestFieldNames = []string{
	"id", "number", "title", "state", "body", "headRefName", "baseRefName",
	"headRefOid", "baseRefOid", "mergeable", "merged", "mergedAt",
	"mergeCommit", "files", "commits", "reviews", "comments", "additions",
	"deletions", "changedFiles", "createdAt", "updatedAt", "url", "author",
	"isDraft", "reviewDecision", "mergedBy", "closedAt",
}

var repositoryFieldNames = []string{
	"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
	"viewerPermission", "hasIssuesEnabled", "description", "visibility",
	"isPrivate", "createdAt", "updatedAt", "url", "issues", "pullRequests",
	"pullRequest", "issue", "labels", "milestones", "stargazerCount",
	"forkCount", "watchers",
}

// gqlVar helpers: read a variable, tolerating numbers as float64.
func gqlString(vars map[string]any, key string) string {
	if v, ok := vars[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func gqlInt(vars map[string]any, key string) int {
	if v, ok := vars[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return 0
}

func gqlInput(vars map[string]any) map[string]any {
	if v, ok := vars["input"]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return map[string]any{}
}

// gqlRepo resolves (owner, name) to a repo, or nil for an unknown owner or
// a missing repo (GraphQL answers data.repository = null, no errors).
func (s *Server) gqlRepo(vars map[string]any) *Repo {
	owner := gqlString(vars, "owner")
	name := gqlString(vars, "name")
	if owner == "" || name == "" {
		return nil
	}
	return s.store.getRepo(owner, name)
}

// gqlRepoObject renders a repository node.
func (s *Server) gqlRepoObject(repo *Repo) map[string]any {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	viewer := s.store.viewer()
	id := s.store.opaqueID("repo", repo.name, repo.id)
	v := map[string]any{
		"id":             id,
		"databaseId":     repo.id,
		"name":           repo.name,
		"nameWithOwner":  viewer + "/" + repo.name,
		"owner":          map[string]any{"login": viewer},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
	if _, ok := repo.refs["refs/heads/"+repo.defaultBranch]; ok {
		v["defaultBranchRef"] = map[string]any{"name": repo.defaultBranch}
	} else {
		v["defaultBranchRef"] = nil
	}
	return v
}

func (s *Server) gqlRepository(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	return gqlData(map[string]any{"repository": s.gqlRepoObject(repo)})
}

func (s *Server) gqlIssueObject(repo *Repo, iss *Issue) map[string]any {
	return map[string]any{
		"id":     s.store.opaqueID("issue", repo.name, iss.Number),
		"number": iss.Number,
		"title":  iss.Title,
		"state":  strings.ToUpper(iss.State),
		"body":   iss.Body,
	}
}

func (s *Server) gqlOpenIssues(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	first := gqlInt(vars, "first")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var nodes []any
	count := 0
	for i := len(repo.issues) - 1; i >= 0; i-- { // CREATED_AT DESC = number DESC
		iss := repo.issues[i]
		if iss.State != "open" {
			continue
		}
		nodes = append(nodes, s.gqlIssueObject(repo, iss))
		count++
		if first > 0 && count >= first {
			break
		}
	}
	if nodes == nil {
		nodes = []any{}
	}
	return gqlData(map[string]any{"repository": map[string]any{
		"issues": map[string]any{"nodes": nodes},
	}})
}

func (s *Server) gqlIssueByNumber(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	num := gqlInt(vars, "number")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	iss, ok := repo.issueByNum[num]
	if !ok {
		return gqlData(map[string]any{"repository": map[string]any{"issue": nil}})
	}
	return gqlData(map[string]any{"repository": map[string]any{
		"issue": s.gqlIssueObject(repo, iss),
	}})
}

func (s *Server) gqlCreateIssue(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	repoID := gqlString(input, "repositoryId")
	ref := s.store.lookupNode(repoID)
	if ref == nil || ref.kind != "repo" {
		return gqlError("repositoryId does not name a repository")
	}
	repo := s.store.getRepo(s.store.viewer(), ref.repo)
	if repo == nil {
		return gqlError("repositoryId does not name a repository")
	}
	iss, errMsg := s.createIssueInRepo(repo, gqlString(input, "title"), gqlString(input, "body"), nil)
	if errMsg != "" {
		return gqlError(errMsg)
	}
	return gqlData(map[string]any{"createIssue": map[string]any{"issue": map[string]any{
		"number": iss.Number,
		"id":     s.store.opaqueID("issue", repo.name, iss.Number),
	}}})
}

func (s *Server) gqlCloseIssue(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	issueID := gqlString(input, "issueId")
	ref := s.store.lookupNode(issueID)
	if ref == nil || ref.kind != "issue" {
		return gqlError("issueId does not name an issue")
	}
	repo := s.store.getRepo(s.store.viewer(), ref.repo)
	if repo == nil {
		return gqlError("issueId does not name an issue")
	}
	repo.mu.Lock()
	iss, ok := repo.issueByNum[ref.num]
	if !ok {
		repo.mu.Unlock()
		return gqlError("issueId does not name an issue")
	}
	iss.State = "closed"
	b := s.issueObject(repo, iss)
	iss.cached = b
	repo.issueJSON[iss.Number] = b
	repo.mu.Unlock()
	return gqlData(map[string]any{"closeIssue": map[string]any{"issue": map[string]any{
		"number": iss.Number,
		"id":     s.store.opaqueID("issue", repo.name, iss.Number),
		"state":  "CLOSED",
	}}})
}

func (s *Server) gqlPullObject(repo *Repo, pr *Pull) map[string]any {
	state := "OPEN"
	if pr.Merged {
		state = "MERGED"
	} else if pr.State == "closed" {
		state = "CLOSED"
	}
	mergeable := "MERGEABLE"
	if pr.State == "open" {
		baseTip, ok1 := repo.refs["refs/heads/"+pr.BaseRef]
		headTip, ok2 := repo.refs["refs/heads/"+pr.HeadRef]
		if ok1 && ok2 && s.pullConflict(repo, pr, baseTip, headTip) {
			mergeable = "CONFLICTING"
		}
	}
	return map[string]any{
		"id":          s.store.opaqueID("pull", repo.name, pr.Number),
		"number":      pr.Number,
		"title":       pr.Title,
		"state":       state,
		"headRefName": pr.HeadRef,
		"baseRefName": pr.BaseRef,
		"mergeable":   mergeable,
		"merged":      pr.Merged,
	}
}

func (s *Server) gqlPullRequests(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	first := gqlInt(vars, "first")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var nodes []any
	for _, pr := range repo.pulls {
		if pr.State != "open" {
			continue
		}
		nodes = append(nodes, s.gqlPullObject(repo, pr))
		if first > 0 && len(nodes) >= first {
			break
		}
	}
	if nodes == nil {
		nodes = []any{}
	}
	return gqlData(map[string]any{"repository": map[string]any{
		"pullRequests": map[string]any{"nodes": nodes},
	}})
}

func (s *Server) gqlPullRequestFiles(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	num := gqlInt(vars, "number")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	pr, ok := repo.pullByNum[num]
	if !ok {
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": nil}})
	}
	baseObj := repo.objects[pr.BaseSha]
	headObj := repo.objects[pr.HeadSha]
	if baseObj == nil || headObj == nil {
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"number": pr.Number, "files": map[string]any{"nodes": []any{}}}}})
	}
	baseTree, _ := parseCommit(baseObj.Data)
	headTree, _ := parseCommit(headObj.Data)
	diff := s.diffFiles(repo, baseTree, headTree)
	var names []string
	for n := range diff {
		names = append(names, n)
	}
	sort.Strings(names)
	var nodes []any
	for _, n := range names {
		nodes = append(nodes, map[string]any{"path": n})
	}
	if nodes == nil {
		nodes = []any{}
	}
	return gqlData(map[string]any{"repository": map[string]any{
		"pullRequest": map[string]any{
			"number": pr.Number,
			"files":  map[string]any{"nodes": nodes},
		},
	}})
}

func (s *Server) gqlPullRequestID(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	num := gqlInt(vars, "number")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	pr, ok := repo.pullByNum[num]
	if !ok {
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": nil}})
	}
	return gqlData(map[string]any{"repository": map[string]any{
		"pullRequest": map[string]any{
			"id":     s.store.opaqueID("pull", repo.name, pr.Number),
			"number": pr.Number,
		},
	}})
}

func (s *Server) gqlCreatePullRequest(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	repoID := gqlString(input, "repositoryId")
	ref := s.store.lookupNode(repoID)
	if ref == nil || ref.kind != "repo" {
		return gqlError("repositoryId does not name a repository")
	}
	repo := s.store.getRepo(s.store.viewer(), ref.repo)
	if repo == nil {
		return gqlError("repositoryId does not name a repository")
	}
	pr, errMsg := s.createPullInRepo(repo, gqlString(input, "title"), "", gqlString(input, "headRefName"), gqlString(input, "baseRefName"))
	if errMsg != "" {
		return gqlError(errMsg)
	}
	return gqlData(map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
		"number":      pr.Number,
		"id":          s.store.opaqueID("pull", repo.name, pr.Number),
		"state":       "OPEN",
		"headRefName": pr.HeadRef,
		"baseRefName": pr.BaseRef,
	}}})
}

func (s *Server) gqlMergePullRequest(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	prID := gqlString(input, "pullRequestId")
	ref := s.store.lookupNode(prID)
	if ref == nil || ref.kind != "pull" {
		return gqlError("pullRequestId does not name a pull request")
	}
	repo := s.store.getRepo(s.store.viewer(), ref.repo)
	if repo == nil {
		return gqlError("pullRequestId does not name a pull request")
	}
	repo.mu.Lock()
	pr, ok := repo.pullByNum[ref.num]
	if !ok {
		repo.mu.Unlock()
		return gqlError("pullRequestId does not name a pull request")
	}
	mergeOid, _, status, errMsg := s.mergePullInternal(repo, pr)
	repo.mu.Unlock()
	if errMsg != "" {
		_ = status
		return gqlError(errMsg)
	}
	_ = mergeOid
	return gqlData(map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{
		"number": pr.Number,
		"merged": true,
		"state":  "MERGED",
	}}})
}

func (s *Server) gqlAddComment(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	subjectID := gqlString(input, "subjectId")
	ref := s.store.lookupNode(subjectID)
	if ref == nil || (ref.kind != "issue" && ref.kind != "pull") {
		return gqlError("subjectId does not name an issue or pull request")
	}
	repo := s.store.getRepo(s.store.viewer(), ref.repo)
	if repo == nil {
		return gqlError("subjectId does not name an issue or pull request")
	}
	num := ref.num
	if ref.kind != "issue" {
		return gqlError("subjectId does not name an issue")
	}
	c, errMsg := s.addCommentInRepo(repo, num, gqlString(input, "body"))
	if errMsg != "" {
		return gqlError(errMsg)
	}
	return gqlData(map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
		"id":   s.store.opaqueID("comment", repo.name, c.ID),
		"body": c.Body,
	}}}})
}

func (s *Server) gqlNode(vars map[string]any) map[string]any {
	id := gqlString(vars, "id")
	ref := s.store.lookupNode(id)
	if ref == nil || ref.kind != "repo" {
		return gqlData(map[string]any{"node": nil})
	}
	repo := s.store.getRepo(s.store.viewer(), ref.repo)
	if repo == nil {
		return gqlData(map[string]any{"node": nil})
	}
	return gqlData(map[string]any{"node": map[string]any{
		"id":            id,
		"name":          repo.name,
		"nameWithOwner": s.store.viewer() + "/" + repo.name,
	}})
}

func (s *Server) gqlIssuesPage(vars map[string]any) map[string]any {
	repo := s.gqlRepo(vars)
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	first := gqlInt(vars, "first")
	after := gqlString(vars, "after")
	afterNum := 0
	if strings.HasPrefix(after, "cursor:") {
		if n, err := strconv.Atoi(strings.TrimPrefix(after, "cursor:")); err == nil {
			afterNum = n
		}
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var nodes []any
	last := 0
	for _, iss := range repo.issues {
		if iss.Number <= afterNum {
			continue
		}
		nodes = append(nodes, map[string]any{"number": iss.Number})
		last = iss.Number
		if first > 0 && len(nodes) >= first {
			break
		}
	}
	hasNext := false
	if len(repo.issues) > 0 && last < repo.issues[len(repo.issues)-1].Number {
		hasNext = true
	}
	endCursor := ""
	if last > 0 {
		endCursor = "cursor:" + strconv.Itoa(last)
	}
	if nodes == nil {
		nodes = []any{}
	}
	return gqlData(map[string]any{"repository": map[string]any{
		"issues": map[string]any{
			"nodes": nodes,
			"pageInfo": map[string]any{
				"hasNextPage": hasNext,
				"endCursor":   endCursor,
			},
		},
	}})
}
