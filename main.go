// forgejo-gitlab-shim
// Exposes the two GitLab REST endpoints Nuxt Studio's GitLab provider uses,
// and translates them to the Forgejo (Gitea-compatible) API.
//
//	GET  /api/v4/projects/:id/repository/files/:path?ref=   -> GET  /api/v1/repos/{o}/{r}/contents/{path}?ref=
//	POST /api/v4/projects/:id/repository/commits            -> POST /api/v1/repos/{o}/{r}/contents
//
// Web links Studio builds (/-/commit, /-/tree, /-/blob) are redirected to Forgejo's UI.
// Standard library only.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	port             string
	forgejoURL       string // where the shim reaches Forgejo, e.g. http://forgejo:3000
	forgejoPublicURL string // what browsers use, for redirects
	allowedOrigins   []string
	allowedRepos     []string // lower-cased "owner/repo"; empty = all
	allowedBranches  []string // commits are refused on any other branch
	allowedPaths     []string // commit paths must start with one of these ("content/")
	committer        identity // git committer of every commit (the bot account)
	maxBodyBytes     int64
	logRequests      bool
}

var (
	cfg    config
	client = &http.Client{Timeout: 5 * time.Minute}
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	cfg = loadConfig()

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           http.HandlerFunc(handle), // no ServeMux: it would clean/redirect encoded paths
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("forgejo-gitlab-shim listening on :%s -> %s\n", cfg.port, cfg.forgejoURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// healthcheck lets the container check itself without a shell or wget in the image.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func loadConfig() config {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
		os.Exit(1)
	}
	c := config{
		port:        envOr("PORT", "8080"),
		forgejoURL:  trimSlash(os.Getenv("FORGEJO_URL")),
		logRequests: os.Getenv("LOG_REQUESTS") != "false",
	}
	if c.forgejoURL == "" {
		fail("Missing required env var FORGEJO_URL")
	}
	c.forgejoPublicURL = trimSlash(envOr("FORGEJO_PUBLIC_URL", c.forgejoURL))
	c.allowedOrigins = list(os.Getenv("ALLOWED_ORIGINS"))
	for _, r := range list(os.Getenv("ALLOWED_REPOS")) {
		c.allowedRepos = append(c.allowedRepos, strings.ToLower(r))
	}
	c.allowedBranches = list(envOr("ALLOWED_BRANCHES", "main"))
	for _, p := range list(envOr("ALLOWED_PATHS", "content/")) {
		dir := strings.TrimSuffix(p, "/")
		if checkPath(dir) != nil {
			fail("ALLOWED_PATHS: %q is not a clean relative directory", p)
		}
		c.allowedPaths = append(c.allowedPaths, dir+"/")
	}
	c.committer = identity{os.Getenv("COMMITTER_NAME"), os.Getenv("COMMITTER_EMAIL")}
	if c.committer.Name == "" || c.committer.Email == "" {
		fail("Missing required env vars COMMITTER_NAME and COMMITTER_EMAIL (the bot account's git identity)")
	}
	mb, err := strconv.ParseFloat(envOr("MAX_BODY_MB", "25"), 64)
	if err != nil || mb <= 0 {
		fail("MAX_BODY_MB must be a positive number")
	}
	c.maxBodyBytes = int64(mb * 1024 * 1024)
	if _, err := strconv.Atoi(c.port); err != nil {
		fail("PORT must be a number")
	}
	return c
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
func trimSlash(s string) string { return strings.TrimRight(s, "/") }
func list(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
func encodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }
func herr(status int, format string, a ...any) error {
	return &httpError{status, fmt.Sprintf(format, a...)}
}

// marshal encodes JSON without escaping <, > and & (matches JSON.stringify).
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// ---------- Forgejo client ----------

var authRe = regexp.MustCompile(`(?i)^(?:bearer|token)\s+(.+)$`)

// Studio sends `PRIVATE-TOKEN: glpat-...` or `Authorization: Bearer <token>`.
// Forgejo wants `Authorization: token <token>`.
func forgejoAuth(r *http.Request) (string, error) {
	if priv := r.Header.Get("Private-Token"); priv != "" {
		return "token " + priv, nil
	}
	if m := authRe.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		return "token " + m[1], nil
	}
	return "", herr(401, "401 Unauthorized")
}

type forgejoResp struct {
	status int
	body   []byte
}

func (r forgejoResp) ok() bool { return r.status >= 200 && r.status < 300 }

func forgejo(ctx context.Context, auth, method, path string, payload any) (forgejoResp, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(marshal(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.forgejoURL+"/api/v1"+path, body)
	if err != nil {
		return forgejoResp{}, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return forgejoResp{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return forgejoResp{}, err
	}
	return forgejoResp{res.StatusCode, b}, nil
}

func forgejoError(r forgejoResp, context string) error {
	var d struct {
		Message string   `json:"message"`
		Errors  []string `json:"errors"`
	}
	msg := ""
	if err := json.Unmarshal(r.body, &d); err != nil {
		msg = string(r.body)
	} else if d.Message != "" {
		msg = d.Message
	} else if len(d.Errors) > 0 {
		msg = strings.Join(d.Errors, ", ")
	}
	if msg == "" {
		msg = fmt.Sprintf("Forgejo returned %d", r.status)
	}
	// GitLab-ish status codes; Studio only special-cases 404.
	status := 502
	if slices.Contains([]int{400, 401, 403, 404, 409, 422}, r.status) {
		status = r.status
	}
	return herr(status, "%s: %s", context, msg)
}

type contentsEntry struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	Content       string `json:"content"`
	SHA           string `json:"sha"`
	LastCommitSHA string `json:"last_commit_sha"`
}

// getFile returns the Forgejo contents entry for a file, or nil if it doesn't exist / isn't a file.
func getFile(ctx context.Context, auth, owner, repo, path, ref string) (*contentsEntry, error) {
	q := ""
	if ref != "" {
		q = "?ref=" + url.QueryEscape(ref)
	}
	r, err := forgejo(ctx, auth, "GET", repoPath(owner, repo)+"/contents/"+encodePath(path)+q, nil)
	if err != nil {
		return nil, err
	}
	if r.status == 404 {
		return nil, nil
	}
	if !r.ok() {
		return nil, forgejoError(r, "read "+path)
	}
	if b := bytes.TrimSpace(r.body); len(b) > 0 && b[0] == '[' {
		return nil, nil // directory listing
	}
	var f contentsEntry
	if err := json.Unmarshal(r.body, &f); err != nil {
		return nil, fmt.Errorf("read %s: bad Forgejo response: %w", path, err)
	}
	if f.Type != "file" {
		return nil, nil // symlink, submodule
	}
	return &f, nil
}

// ---------- GitLab endpoint handlers ----------

func parseProject(rawID string) (owner, repo string, err error) {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		return "", "", herr(404, "404 Project Not Found")
	}
	slash := strings.LastIndex(id, "/")
	if slash <= 0 {
		return "", "", herr(404, `404 Project Not Found (shim needs "owner/repo", not a numeric id)`)
	}
	owner, repo = id[:slash], id[slash+1:]
	if strings.Contains(owner, "/") {
		return "", "", herr(404, "404 Project Not Found (Forgejo has no nested groups)")
	}
	if len(cfg.allowedRepos) > 0 && !slices.Contains(cfg.allowedRepos, strings.ToLower(owner+"/"+repo)) {
		return "", "", herr(404, "404 Project Not Found")
	}
	return owner, repo, nil
}

type gitlabFile struct {
	FileName     string `json:"file_name"`
	FilePath     string `json:"file_path"`
	Size         int64  `json:"size"`
	Encoding     string `json:"encoding"`
	Content      string `json:"content"`
	Ref          string `json:"ref"`
	BlobID       string `json:"blob_id"`
	CommitID     string `json:"commit_id"`
	LastCommitID string `json:"last_commit_id"`
}

func handleGetFile(r *http.Request, rawID, rawPath string) (any, error) {
	auth, err := forgejoAuth(r)
	if err != nil {
		return nil, err
	}
	owner, repo, err := parseProject(rawID)
	if err != nil {
		return nil, err
	}
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return nil, herr(400, "invalid file path")
	}
	ref := r.URL.Query().Get("ref")
	f, err := getFile(r.Context(), auth, owner, repo, path, ref)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, herr(404, "404 File Not Found")
	}
	return gitlabFile{
		FileName:     f.Name,
		FilePath:     f.Path,
		Size:         f.Size,
		Encoding:     "base64",
		Content:      stripNewlines(f.Content),
		Ref:          ref,
		BlobID:       f.SHA,
		CommitID:     f.LastCommitSHA,
		LastCommitID: f.LastCommitSHA,
	}, nil
}

func stripNewlines(s string) string { return strings.ReplaceAll(s, "\n", "") }

func toBase64(content *string, encoding string) string {
	if content == nil {
		return ""
	}
	c := *content
	if encoding == "base64" {
		if i := strings.Index(c, ";base64,"); strings.HasPrefix(c, "data:") && i != -1 {
			return c[i+8:]
		}
		return c
	}
	return base64.StdEncoding.EncodeToString([]byte(c))
}

type gitlabAction struct {
	Action       string  `json:"action"`
	FilePath     string  `json:"file_path"`
	PreviousPath string  `json:"previous_path"`
	Content      *string `json:"content"`
	Encoding     string  `json:"encoding"`
}

type commitRequest struct {
	Branch        string         `json:"branch"`
	CommitMessage string         `json:"commit_message"`
	Actions       []gitlabAction `json:"actions"`
	AuthorName    string         `json:"author_name"`
	AuthorEmail   string         `json:"author_email"`
}

type fileOp struct {
	Operation string  `json:"operation"`
	Path      string  `json:"path"`
	FromPath  string  `json:"from_path,omitempty"`
	Content   *string `json:"content,omitempty"`
	SHA       string  `json:"sha,omitempty"`
}

type identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type commitPayload struct {
	Branch    string    `json:"branch"`
	Message   string    `json:"message"`
	Files     []fileOp  `json:"files"`
	Author    *identity `json:"author,omitempty"`
	Committer *identity `json:"committer"`
}

type gitlabCommit struct {
	ID          string `json:"id,omitempty"`
	ShortID     string `json:"short_id"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	AuthorName  string `json:"author_name,omitempty"`
	AuthorEmail string `json:"author_email,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	WebURL      string `json:"web_url"`
}

// checkPath rejects anything that isn't a plain relative path: no "..", ".", empty
// segments, absolute paths, backslashes, control characters or .git directories.
// It checks the raw string, so there is nothing for a later normalisation step to reinterpret.
func checkPath(p string) error {
	if p == "" {
		return herr(400, "file_path is required")
	}
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') {
		return herr(403, "path %q is not allowed: must be relative, with forward slashes", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return herr(403, "path %q is not allowed: control character", p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return herr(403, "path %q is not allowed: segment %q", p, seg)
		}
	}
	return nil
}

// checkWritePath allows a commit to touch p only if it is a clean path inside an allowed directory.
func checkWritePath(p string) error {
	if err := checkPath(p); err != nil {
		return err
	}
	for _, prefix := range cfg.allowedPaths {
		if strings.HasPrefix(p, prefix) {
			return nil
		}
	}
	return herr(403, "path %q is not allowed: commits may only change files under %s", p, strings.Join(cfg.allowedPaths, ", "))
}

// checkCommit refuses the whole request before anything reaches Forgejo.
func checkCommit(body commitRequest) error {
	if !slices.Contains(cfg.allowedBranches, body.Branch) {
		return herr(403, "branch %q is not allowed", body.Branch)
	}
	for _, a := range body.Actions {
		if err := checkWritePath(a.FilePath); err != nil {
			return err
		}
		if a.Action == "move" && a.PreviousPath != "" {
			if err := checkWritePath(a.PreviousPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveAction turns one GitLab action into a Forgejo file operation (nil = nothing to do).
func resolveAction(ctx context.Context, auth, owner, repo, branch string, a gitlabAction) (*fileOp, error) {
	path := a.FilePath // already checked by checkCommit
	existing, err := getFile(ctx, auth, owner, repo, path, branch)
	if err != nil {
		return nil, err
	}
	switch a.Action {
	case "create", "update":
		content := toBase64(a.Content, a.Encoding)
		// Treat create/update as upsert so a stale draft status can't wedge a publish.
		if existing != nil {
			return &fileOp{Operation: "update", Path: path, Content: &content, SHA: existing.SHA}, nil
		}
		return &fileOp{Operation: "create", Path: path, Content: &content}, nil
	case "delete":
		// Already gone: nothing to do (GitLab would error; skipping is friendlier).
		if existing == nil {
			return nil, nil
		}
		return &fileOp{Operation: "delete", Path: path, SHA: existing.SHA}, nil
	case "move":
		if a.PreviousPath == "" {
			return nil, herr(400, "move requires previous_path")
		}
		from, err := getFile(ctx, auth, owner, repo, a.PreviousPath, branch)
		if err != nil {
			return nil, err
		}
		if from == nil {
			return nil, herr(400, "move: %s does not exist", a.PreviousPath)
		}
		content := stripNewlines(from.Content)
		if a.Content != nil {
			content = toBase64(a.Content, a.Encoding)
		}
		return &fileOp{Operation: "update", Path: path, FromPath: a.PreviousPath, Content: &content, SHA: from.SHA}, nil
	default:
		return nil, herr(400, `unsupported action "%s"`, a.Action)
	}
}

func handleCommit(w http.ResponseWriter, r *http.Request, rawID string) (any, error) {
	auth, err := forgejoAuth(r)
	if err != nil {
		return nil, err
	}
	owner, repo, err := parseProject(rawID)
	if err != nil {
		return nil, err
	}
	var body commitRequest
	if err := readJSON(w, r, &body); err != nil {
		return nil, err
	}
	if body.Branch == "" || body.CommitMessage == "" || len(body.Actions) == 0 {
		return nil, herr(400, "branch, commit_message and actions are required")
	}
	if err := checkCommit(body); err != nil {
		return nil, err
	}

	// Forgejo needs the current blob sha for update/delete. Look them up in parallel.
	ops := make([]*fileOp, len(body.Actions))
	errs := make([]error, len(body.Actions))
	var wg sync.WaitGroup
	for i, a := range body.Actions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ops[i], errs[i] = resolveAction(r.Context(), auth, owner, repo, body.Branch, a)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, e := range errs {
			if e != nil {
				return nil, e // first failing action, in request order
			}
		}
	}
	files := make([]fileOp, 0, len(ops))
	for _, op := range ops {
		if op != nil {
			files = append(files, *op)
		}
	}
	if len(files) == 0 {
		return nil, herr(400, "nothing to commit (all deleted files were already absent)")
	}

	// The editor is the author; the committer is always the configured bot identity, whatever the request says.
	committer := cfg.committer
	payload := commitPayload{Branch: body.Branch, Message: body.CommitMessage, Files: files, Author: &committer, Committer: &committer}
	if body.AuthorName != "" && body.AuthorEmail != "" {
		payload.Author = &identity{body.AuthorName, body.AuthorEmail}
	}
	res, err := forgejo(r.Context(), auth, "POST", repoPath(owner, repo)+"/contents", payload)
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return nil, forgejoError(res, "commit")
	}

	var out struct {
		Commit struct {
			SHA     string `json:"sha"`
			Created string `json:"created"`
			Author  struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	json.Unmarshal(res.body, &out)
	c := out.Commit
	short := c.SHA
	if len(short) > 8 {
		short = short[:8]
	}
	title, _, _ := strings.Cut(body.CommitMessage, "\n")
	return gitlabCommit{
		ID:          c.SHA,
		ShortID:     short,
		Title:       title,
		Message:     body.CommitMessage,
		AuthorName:  firstNonEmpty(c.Author.Name, body.AuthorName),
		AuthorEmail: firstNonEmpty(c.Author.Email, body.AuthorEmail),
		CreatedAt:   firstNonEmpty(c.Created, c.Author.Date),
		WebURL:      fmt.Sprintf("%s/%s/%s/commit/%s", cfg.forgejoPublicURL, owner, repo, c.SHA),
	}, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var webRe = regexp.MustCompile(`^/([^/]+)/([^/]+)(?:/-/(commit|tree|blob)/(.+))?/?$`)

// webRedirect maps GitLab-style web URLs to Forgejo web URLs.
func webRedirect(pathname, search string) string {
	m := webRe.FindStringSubmatch(pathname)
	if m == nil {
		return cfg.forgejoPublicURL + pathname + search
	}
	owner, repo, kind, rest := m[1], m[2], m[3], m[4]
	base := cfg.forgejoPublicURL + "/" + owner + "/" + repo
	switch kind {
	case "":
		return base
	case "commit":
		return base + "/commit/" + rest
	default:
		return base + "/src/branch/" + rest // tree/<branch> and blob/<branch>/<path>
	}
}

// ---------- HTTP plumbing ----------

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return herr(413, "request body larger than %d bytes", cfg.maxBodyBytes)
		}
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return herr(400, "invalid JSON body")
	}
	return nil
}

func corsHeaders(r *http.Request) map[string]string {
	origin := r.Header.Get("Origin")
	if origin == "" || len(cfg.allowedOrigins) == 0 {
		return nil
	}
	if !slices.Contains(cfg.allowedOrigins, "*") && !slices.Contains(cfg.allowedOrigins, origin) {
		return nil
	}
	return map[string]string{
		"Access-Control-Allow-Origin":  origin,
		"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
		"Access-Control-Allow-Headers": "authorization, private-token, content-type",
		"Access-Control-Max-Age":       "600",
		"Vary":                         "Origin",
	}
}

func writeHead(w http.ResponseWriter, status int, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
}

func sendJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	writeHead(w, status, headers)
	w.Write(marshal(body))
}

var (
	filesRe   = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/files/([^/]+)$`)
	commitsRe = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/commits$`)
)

func handle(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	// Keep the raw (still-encoded) path: GitLab encodes "/" inside ids and file paths as %2F.
	rawPath, _, _ := strings.Cut(r.RequestURI, "?")
	if rawPath == "" {
		rawPath = "/"
	}
	status := route(w, r, rawPath, corsHeaders(r))
	if cfg.logRequests {
		fmt.Printf("%s %s %d %dms\n", r.Method, rawPath, status, time.Since(started).Milliseconds())
	}
}

func route(w http.ResponseWriter, r *http.Request, rawPath string, cors map[string]string) int {
	status, err := dispatch(w, r, rawPath, cors)
	if err == nil {
		return status
	}
	var he *httpError
	msg := err.Error()
	if errors.As(err, &he) {
		status = he.status
	} else {
		status = 502
		fmt.Fprintln(os.Stderr, err)
		msg = "upstream request failed" // don't leak internal addresses to the browser
	}
	sendJSON(w, status, map[string]string{"message": msg}, cors)
	return status
}

func dispatch(w http.ResponseWriter, r *http.Request, rawPath string, cors map[string]string) (int, error) {
	if r.Method == http.MethodOptions {
		writeHead(w, 204, cors)
		return 204, nil
	}
	if rawPath == "/healthz" {
		sendJSON(w, 200, map[string]bool{"ok": true}, cors)
		return 200, nil
	}
	if r.Method == http.MethodGet {
		if m := filesRe.FindStringSubmatch(rawPath); m != nil {
			body, err := handleGetFile(r, m[1], m[2])
			if err != nil {
				return 0, err
			}
			sendJSON(w, 200, body, cors)
			return 200, nil
		}
	}
	if r.Method == http.MethodPost {
		if m := commitsRe.FindStringSubmatch(rawPath); m != nil {
			body, err := handleCommit(w, r, m[1])
			if err != nil {
				return 0, err
			}
			sendJSON(w, 201, body, cors)
			return 201, nil
		}
	}
	if strings.HasPrefix(rawPath, "/api/") {
		return 0, herr(404, "404 Not Found (shim does not implement %s %s)", r.Method, rawPath)
	}
	if r.Method == http.MethodGet {
		search := ""
		if r.URL.RawQuery != "" {
			search = "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", webRedirect(rawPath, search))
		w.WriteHeader(302)
		return 302, nil
	}
	return 0, herr(405, "method not allowed")
}
