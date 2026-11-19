package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// MCP (Streamable HTTP, revision 2026-07-28): server/discover, initialize,
// tools/list, tools/call. Every request is a JSON-RPC 2.0 object; the id is
// echoed exactly. A malformed body answers HTTP 400 with JSON-RPC -32700.

var mcpIDRe = regexp.MustCompile(`"id"\s*:\s*("[^"]*"|-?[0-9]+)`)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Parse error: HTTP 400, JSON-RPC -32700, id extracted from the
		// raw body when possible.
		id := json.RawMessage("null")
		if m := mcpIDRe.FindSubmatch(body); m != nil {
			id = m[1]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(mcpErrorResponse(id, -32700, "Parse error"))
		return
	}
	if req.Method == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(mcpErrorResponse(rawID(req.ID), -32600, "Invalid Request"))
		return
	}
	// A notification (no id) gets no JSON-RPC response.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "server/discover":
		s.mcpResult(w, req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": serverName, "version": serverVersion},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": serverName, "version": serverVersion},
			},
		})
	case "initialize":
		s.mcpResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})
	case "ping":
		s.mcpResult(w, req.ID, map[string]any{})
	case "tools/list":
		s.mcpToolsList(w, req.ID)
	case "tools/call":
		s.mcpToolsCall(w, req.ID, req.Params)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mcpErrorResponse(rawID(req.ID), -32601, "Method not found"))
	}
}

func rawID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func mcpErrorResponse(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": msg},
	})
	return b
}

func (s *Server) mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID(id)),
		"result":  result,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------------------
// tools/list: the tools array is precomputed once, so two calls answer the
// same bytes.

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTool(name, desc string, props map[string]any, required []string) mcpToolDef {
	return mcpToolDef{
		Name:        name,
		Description: desc,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		},
	}
}

var mcpToolsJSON = func() []byte {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	boolFn := func() map[string]any { return map[string]any{"type": "boolean"} }
	integer := func() map[string]any { return map[string]any{"type": "integer"} }
	tools := []mcpToolDef{
		mcpTool("create_repo", "Create a repository owned by the authenticated user.",
			map[string]any{"name": str(), "description": str(), "private": boolFn()}, []string{"name"}),
		mcpTool("get_repo", "Get a repository by owner and name.",
			map[string]any{"owner": str(), "repo": str()}, []string{"owner", "repo"}),
		mcpTool("list_repos", "List the authenticated user's repositories.",
			map[string]any{}, []string{}),
		mcpTool("create_issue", "Create an issue in a repository.",
			map[string]any{"owner": str(), "repo": str(), "title": str(), "body": str()}, []string{"owner", "repo", "title"}),
		mcpTool("get_issue", "Get one issue by number.",
			map[string]any{"owner": str(), "repo": str(), "issue_number": integer()}, []string{"owner", "repo", "issue_number"}),
		mcpTool("list_issues", "List issues in a repository.",
			map[string]any{"owner": str(), "repo": str(), "state": str()}, []string{"owner", "repo"}),
		mcpTool("create_pull_request", "Create a pull request.",
			map[string]any{"owner": str(), "repo": str(), "title": str(), "head": str(), "base": str(), "body": str()},
			[]string{"owner", "repo", "title", "head", "base"}),
		mcpTool("merge_pull_request", "Merge a pull request.",
			map[string]any{"owner": str(), "repo": str(), "pull_number": integer()}, []string{"owner", "repo", "pull_number"}),
	}
	b, _ := json.Marshal(tools)
	return b
}()

func (s *Server) mcpToolsList(w http.ResponseWriter, id json.RawMessage) {
	// The tools array is embedded verbatim; only the id varies.
	resp := []byte(`{"jsonrpc":"2.0","id":`)
	resp = append(resp, rawID(id)...)
	resp = append(resp, []byte(`,"result":{"tools":`)...)
	resp = append(resp, mcpToolsJSON...)
	resp = append(resp, []byte(`}}`)...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// ---------------------------------------------------------------------------
// tools/call

type mcpCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func mcpToolResult(text string, structured any) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": structured,
		"isError":           false,
	}
}

func mcpToolError(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": true,
	}
}

func mcpArgString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, key string) int {
	if v, ok := args[key]; ok {
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

func (s *Server) mcpToolsCall(w http.ResponseWriter, id json.RawMessage, paramsRaw json.RawMessage) {
	var params mcpCallParams
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mcpErrorResponse(rawID(id), -32602, "Invalid params"))
		return
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	// The tool's required arguments, by name.
	required := map[string][]string{
		"create_repo":         {"name"},
		"get_repo":            {"owner", "repo"},
		"list_repos":          {},
		"create_issue":        {"owner", "repo", "title"},
		"get_issue":           {"owner", "repo", "issue_number"},
		"list_issues":         {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":  {"owner", "repo", "pull_number"},
	}
	req, known := required[params.Name]
	if !known {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mcpErrorResponse(rawID(id), -32601, "Method not found"))
		return
	}
	for _, key := range req {
		v, ok := args[key]
		if !ok || v == nil || v == "" {
			s.mcpResult(w, id, mcpToolError("missing required argument: "+key))
			return
		}
	}
	owner := mcpArgString(args, "owner")
	repoName := mcpArgString(args, "repo")
	var result map[string]any
	switch params.Name {
	case "create_repo":
		name := mcpArgString(args, "name")
		repo, ok := s.store.createRepo(name, mcpArgString(args, "description"), false)
		if v, ok2 := args["private"]; ok2 {
			if b, isBool := v.(bool); isBool && repo != nil {
				repo.mu.Lock()
				repo.priv = b
				repo.mu.Unlock()
			}
		}
		if !ok {
			result = mcpToolError("repository " + name + " already exists")
			break
		}
		viewer := s.store.viewer()
		result = mcpToolResult("Created repository "+viewer+"/"+name, map[string]any{
			"name": name, "full_name": viewer + "/" + name, "private": repo.priv,
		})
	case "get_repo":
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			result = mcpToolError("repository " + owner + "/" + repoName + " not found")
			break
		}
		repo.mu.RLock()
		desc := repo.desc
		repo.mu.RUnlock()
		result = mcpToolResult("Repository "+owner+"/"+repoName+": "+desc, map[string]any{
			"name": repoName, "full_name": owner + "/" + repoName, "description": desc,
		})
	case "list_repos":
		var names []string
		for _, repo := range s.store.listRepos() {
			names = append(names, repo.name)
		}
		result = mcpToolResult("Repositories: "+strings.Join(names, ", "), map[string]any{"repos": names})
	case "create_issue":
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			result = mcpToolError("repository " + owner + "/" + repoName + " not found")
			break
		}
		iss, errMsg := s.createIssueInRepo(repo, mcpArgString(args, "title"), mcpArgString(args, "body"), nil)
		if errMsg != "" {
			result = mcpToolError(errMsg)
			break
		}
		result = mcpToolResult("Created issue #"+strconv.Itoa(iss.Number)+": "+iss.Title, map[string]any{
			"number": iss.Number, "id": iss.DBID, "title": iss.Title, "state": iss.State,
		})
	case "get_issue":
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			result = mcpToolError("repository " + owner + "/" + repoName + " not found")
			break
		}
		num := mcpArgInt(args, "issue_number")
		repo.mu.RLock()
		b, ok := repo.issueJSON[num]
		repo.mu.RUnlock()
		if !ok {
			result = mcpToolError("issue #" + strconv.Itoa(num) + " not found")
			break
		}
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		result = mcpToolResult(string(b), obj)
	case "list_issues":
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			result = mcpToolError("repository " + owner + "/" + repoName + " not found")
			break
		}
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
		}
		repo.mu.RLock()
		var items []any
		var text strings.Builder
		for _, iss := range repo.issues {
			if state != "all" && iss.State != state {
				continue
			}
			items = append(items, map[string]any{"number": iss.Number, "title": iss.Title, "state": iss.State})
			text.WriteString("{\"number\": " + strconv.Itoa(iss.Number) + ", \"title\": " + strconv.Quote(iss.Title) + "}\n")
		}
		repo.mu.RUnlock()
		if items == nil {
			items = []any{}
		}
		result = mcpToolResult(text.String(), map[string]any{"issues": items})
	case "create_pull_request":
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			result = mcpToolError("repository " + owner + "/" + repoName + " not found")
			break
		}
		pr, errMsg := s.createPullInRepo(repo, mcpArgString(args, "title"), mcpArgString(args, "body"),
			mcpArgString(args, "head"), mcpArgString(args, "base"))
		if errMsg != "" {
			result = mcpToolError(errMsg)
			break
		}
		result = mcpToolResult("Created pull request #"+strconv.Itoa(pr.Number)+": "+pr.Title, map[string]any{
			"number": pr.Number, "title": pr.Title, "state": "OPEN",
			"headRefName": pr.HeadRef, "baseRefName": pr.BaseRef,
		})
	case "merge_pull_request":
		repo := s.store.getRepo(owner, repoName)
		if repo == nil {
			result = mcpToolError("repository " + owner + "/" + repoName + " not found")
			break
		}
		num := mcpArgInt(args, "pull_number")
		repo.mu.Lock()
		pr, ok := repo.pullByNum[num]
		if !ok {
			repo.mu.Unlock()
			result = mcpToolError("pull request #" + strconv.Itoa(num) + " not found")
			break
		}
		mergeOid, _, _, errMsg := s.mergePullInternal(repo, pr)
		repo.mu.Unlock()
		if errMsg != "" {
			result = mcpToolError(errMsg)
			break
		}
		result = mcpToolResult("Merged pull request #"+strconv.Itoa(num), map[string]any{
			"number": num, "merged": true, "sha": mergeOid,
		})
	}
	s.mcpResult(w, id, result)
}
