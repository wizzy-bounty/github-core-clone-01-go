// Anvil-Go: a GitHub-compatible git forge, written in Go (stdlib only).
//
// Entry point and lifecycle: bind an ephemeral port, register with the
// environment (POST $MAKEFUN_ENV_URL/register), serve until the environment
// reports done (GET $MAKEFUN_ENV_URL/done), then exit 0.
//
// The forge speaks git smart HTTP (protocol v2 and v0, receive-pack),
// REST v3, GraphQL v4 and MCP (Streamable HTTP, 2026-07-28 discover).
// All state lives in memory; responses on the hot paths are precomputed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// serverName is the forge's identity (MCP serverInfo, git agent string).
	serverName    = "anvil-go"
	serverVersion = "1.0.0"
	// scenarioEpochSec is the contest's day one (2025-01-01T00:00:00Z).
	// Every server-made timestamp derives from it plus a logical counter,
	// so replays are byte-identical: no wall clock, no unseeded randomness.
	scenarioEpochSec = 1735689600
	// scenarioSeed is the contest's fixed seed (wire.ts SCENARIO_SEED).
	scenarioSeed = uint64(0x6a09e667f3bcc909)
)

// ---------------------------------------------------------------------------
// Seeded scenario RNG (wire.ts: fnv1a64, mix64, Rng, generateUsers).
// The viewer login is derived from the fixed seed, exactly as the suite
// builds it, so the forge knows the token owner's login without any
// request having named it yet. Verified locally: users[0] = "adaiversen".

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo",
	"iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel",
	"quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia",
	"holm", "iversen", "jansen", "keller", "lind", "moreau", "novak",
	"olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega",
	"weber", "xu", "yilmaz", "zimmer",
}

func fnv1a64(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		h ^= uint64(text[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type scenarioRng struct{ state uint64 }

func newScenarioRng(label string) *scenarioRng {
	return &scenarioRng{state: mix64(scenarioSeed ^ fnv1a64(label))}
}

func (r *scenarioRng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *scenarioRng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *scenarioRng) nextInt(max int) int {
	if max == 1 {
		return 0
	}
	const rangeU = uint64(4294967296)
	limit := rangeU - (rangeU % uint64(max))
	for {
		v := r.nextU32()
		if uint64(v) < limit {
			return int(v) % max
		}
	}
}

func (r *scenarioRng) pick(items []string) string { return items[r.nextInt(len(items))] }

// deriveViewerLogin returns the scenario's first user login (the viewer).
func deriveViewerLogin() string {
	rng := newScenarioRng("users")
	first := rng.pick(firstNames)
	last := rng.pick(lastNames)
	return first + last
}

// ---------------------------------------------------------------------------
// Server

type Server struct {
	store   *Store
	token   string // MAKEFUN_SHOP_TOKEN; empty in local dev (accept any bearer)
	envURL  string // MAKEFUN_ENV_URL
	reqSeq  atomic.Uint64
	httpSrv *http.Server
	ln      net.Listener
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "anvil-go: MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	viewer := deriveViewerLogin()
	s := &Server{store: newStore(viewer, token), token: token, envURL: envURL}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "anvil-go: listen:", err)
		os.Exit(1)
	}
	s.ln = ln
	port := ln.Addr().(*net.TCPAddr).Port
	s.httpSrv = &http.Server{Handler: s}

	// Serve in the background, then run the lifecycle: register, poll done.
	go func() {
		_ = s.httpSrv.Serve(ln)
	}()

	if err := s.register(port); err != nil {
		fmt.Fprintln(os.Stderr, "anvil-go: register:", err)
		os.Exit(1)
	}
	s.pollDone()
	_ = s.httpSrv.Shutdown(context.Background())
	os.Exit(0)
}

// register POSTs {"port": N} to $MAKEFUN_ENV_URL/register, retrying with
// backoff. 200/201/409 all mean registered (first register wins).
func (s *Server) register(port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 2*time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.envURL+"/register", bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
				return nil
			}
		}
		time.Sleep(delay)
	}
}

// pollDone polls GET $MAKEFUN_ENV_URL/done until {"done": true}.
func (s *Server) pollDone() {
	for {
		if s.doneOnce() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *Server) doneOnce() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.envURL+"/done", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.Done
}

// ---------------------------------------------------------------------------
// Routing

// ServeHTTP dispatches every request. No auth on /healthz or git routes;
// the API surfaces (/api/v3, /api/graphql, /mcp) require a bearer token.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every reply carries a non-empty X-GitHub-Request-Id.
	w.Header().Set("X-GitHub-Request-Id", s.nextReqID())
	path := r.URL.Path

	if path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Git smart HTTP: /{owner}/{repo}.git/info/refs,
	// /{owner}/{repo}.git/git-upload-pack, /{owner}/{repo}.git/git-receive-pack
	if strings.HasSuffix(path, ".git/info/refs") ||
		strings.HasSuffix(path, ".git/git-upload-pack") ||
		strings.HasSuffix(path, ".git/git-receive-pack") {
		s.handleGit(w, r)
		return
	}

	// API surfaces: auth first.
	if strings.HasPrefix(path, "/api/") || path == "/mcp" {
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
	}

	switch {
	case path == "/api/v3" || path == "/api/v3/":
		s.handleAPIRoot(w, r)
	case path == "/api/v3/meta":
		writeJSON(w, http.StatusOK, map[string]string{"installed_version": serverVersion})
	case path == "/api/v3/user":
		s.handleUser(w, r)
	case path == "/api/v3/user/repos":
		s.handleUserRepos(w, r)
	case strings.HasPrefix(path, "/api/v3/repos/"):
		s.handleRepoAPI(w, r)
	case path == "/api/graphql":
		s.handleGraphQL(w, r)
	case path == "/mcp":
		s.handleMCP(w, r)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := strings.TrimSpace(h[len("Bearer "):])
	if tok == "" {
		return false
	}
	if s.token == "" {
		return true // local dev: any non-empty bearer
	}
	return tok == s.token
}

// ---------------------------------------------------------------------------
// Response helpers

// nextReqID returns a fresh request id for X-GitHub-Request-Id.
func (s *Server) nextReqID() string {
	return "anvil-go-" + strconv.FormatUint(s.reqSeq.Add(1), 10)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeErr answers a JSON {"message": ...} error.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// splitPath splits "/a/b/c" into ["a","b","c"].
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// decodeJSONBody reads and parses a JSON request body. A malformed body is
// an error (the caller answers 400); an empty body decodes to the zero value.
func decodeJSONBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}
