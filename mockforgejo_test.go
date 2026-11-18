package main

// Minimal Forgejo API stand-in backed by a real bare git repo.
// Implements GET/POST /api/v1/repos/{o}/{r}/contents with Forgejo's validation rules:
// token auth, sha required (and must match) for update/delete, create fails if the file exists.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	mockToken  = "testtoken"
	mockOwner  = "kit"
	mockRepo   = "site"
	mockBranch = "main"
)

type mockForgejo struct {
	t      *testing.T
	gitDir string
	work   string
	tmp    string

	commitMu sync.Mutex // one commit at a time, like a real ref update

	authMu sync.Mutex
	auth   []string // every Authorization header received
}

type mockOp struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	FromPath  string `json:"from_path"`
	Content   string `json:"content"`
	SHA       string `json:"sha"`
	buf       []byte
}

func newMockForgejo(t *testing.T) *mockForgejo {
	t.Helper()
	dir := t.TempDir()
	m := &mockForgejo{t: t, gitDir: filepath.Join(dir, "repo.git"), work: filepath.Join(dir, "work"), tmp: dir}
	if err := os.Mkdir(m.work, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", mockBranch, m.gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	seed := []mockOp{
		{Operation: "create", Path: "content/index.md", buf: []byte("# Home\n\nHello\n")},
		{Operation: "create", Path: "content/blog/first.md", buf: []byte("---\ntitle: First\n---\n\nPost\n")},
		{Operation: "create", Path: "nuxt.config.ts", buf: []byte("export default {}\n")},
	}
	if _, err := m.commit(seed, "seed", "Seed", "seed@x"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return m
}

func (m *mockForgejo) run(env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"--git-dir", m.gitDir, "--work-tree", m.work}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

// git runs a git command against the mock's repo for test assertions.
func (m *mockForgejo) git(args ...string) string {
	m.t.Helper()
	out, err := m.run(nil, nil, args...)
	if err != nil {
		m.t.Fatal(err)
	}
	return string(out)
}

func (m *mockForgejo) authHeaders() []string {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	return append([]string(nil), m.auth...)
}

func (m *mockForgejo) headSHA() string {
	out, err := m.run(nil, nil, "rev-parse", "refs/heads/"+mockBranch)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type lsEntry struct{ mode, typ, sha, name string }

func (m *mockForgejo) ls(ref, p string) *lsEntry {
	out, err := m.run(nil, nil, "ls-tree", ref, "--", p)
	if err != nil { // unknown ref -> 404 like Forgejo
		return nil
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil
	}
	meta, name, _ := strings.Cut(line, "\t")
	f := strings.Fields(meta)
	if len(f) != 3 {
		return nil
	}
	return &lsEntry{f[0], f[1], f[2], name}
}

func (m *mockForgejo) commit(ops []mockOp, message, name, email string) (string, error) {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(m.tmp, fmt.Sprintf("index-%d", time.Now().UnixNano()))}
	parent := m.headSHA()
	if parent != "" {
		if _, err := m.run(idx, nil, "read-tree", parent); err != nil {
			return "", err
		}
	}
	for _, op := range ops {
		if op.Operation == "delete" {
			if _, err := m.run(idx, nil, "update-index", "--remove", "--force-remove", op.Path); err != nil {
				return "", err
			}
			continue
		}
		if op.FromPath != "" && op.FromPath != op.Path {
			if _, err := m.run(idx, nil, "update-index", "--force-remove", op.FromPath); err != nil {
				return "", err
			}
		}
		blob, err := m.run(nil, op.buf, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		if _, err := m.run(idx, nil, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(string(blob))+","+op.Path); err != nil {
			return "", err
		}
	}
	tree, err := m.run(idx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(string(tree))}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", message)
	id := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	sha, err := m.run(id, nil, args...)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(sha))
	if _, err := m.run(nil, nil, "update-ref", "refs/heads/"+mockBranch, s); err != nil {
		return "", err
	}
	return s, nil
}

func mockSend(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

var (
	b64Re  = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)
	wrapRe = regexp.MustCompile(`(.{60})`)
)

func (m *mockForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.authMu.Lock()
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	m.authMu.Unlock()

	msg := func(s string) map[string]string { return map[string]string{"message": s} }
	if r.Header.Get("Authorization") != "token "+mockToken {
		mockSend(w, 401, msg("token is required"))
		return
	}
	base := "/api/v1/repos/" + mockOwner + "/" + mockRepo + "/contents"
	if !strings.HasPrefix(r.URL.Path, base) {
		mockSend(w, 404, msg("repo not found"))
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = mockBranch
	}

	switch {
	case r.Method == http.MethodGet:
		p := strings.TrimPrefix(r.URL.Path, base+"/")
		e := m.ls(ref, p)
		if e == nil {
			mockSend(w, 404, msg("object does not exist"))
			return
		}
		if e.typ == "tree" {
			mockSend(w, 200, []map[string]string{{"type": "dir"}})
			return
		}
		buf, err := m.run(nil, nil, "cat-file", "blob", e.sha)
		if err != nil {
			mockSend(w, 500, msg(err.Error()))
			return
		}
		// Real Forgejo doesn't wrap base64; wrapping here checks the shim strips newlines.
		b64 := wrapRe.ReplaceAllString(base64.StdEncoding.EncodeToString(buf), "$1\n")
		mockSend(w, 200, map[string]any{
			"name": path.Base(p), "path": p, "sha": e.sha, "last_commit_sha": m.headSHA(),
			"type": "file", "size": len(buf), "encoding": "base64", "content": b64,
		})

	case r.Method == http.MethodPost && r.URL.Path == base:
		var body struct {
			Branch  string   `json:"branch"`
			Message string   `json:"message"`
			Files   []mockOp `json:"files"`
			Author  *struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mockSend(w, 400, msg("bad json"))
			return
		}
		branch := body.Branch
		if branch == "" {
			branch = mockBranch
		}
		if branch != mockBranch {
			mockSend(w, 404, msg("branch does not exist"))
			return
		}
		ops := make([]mockOp, 0, len(body.Files))
		for _, f := range body.Files {
			switch f.Operation {
			case "create", "update", "delete":
			default:
				mockSend(w, 422, msg("bad operation "+f.Operation))
				return
			}
			target := f.Path
			if f.FromPath != "" {
				target = f.FromPath
			}
			e := m.ls(branch, target)
			if f.Operation == "create" && e != nil {
				mockSend(w, 422, msg(fmt.Sprintf("repository file already exists [path: %s]", f.Path)))
				return
			}
			if f.Operation != "create" {
				if e == nil {
					mockSend(w, 404, msg(fmt.Sprintf("file does not exist [path: %s]", f.Path)))
					return
				}
				if f.SHA == "" {
					mockSend(w, 422, msg("sha is required"))
					return
				}
				if f.SHA != e.sha {
					mockSend(w, 409, msg(fmt.Sprintf("sha does not match [given: %s, expected: %s]", f.SHA, e.sha)))
					return
				}
			}
			buf, err := base64.StdEncoding.DecodeString(f.Content)
			if !b64Re.MatchString(f.Content) || err != nil {
				mockSend(w, 422, msg("invalid base64 content"))
				return
			}
			f.buf = buf
			ops = append(ops, f)
		}
		name, email := "Token User", "token@x"
		if body.Author != nil && body.Author.Name != "" {
			name, email = body.Author.Name, body.Author.Email
		}
		sha, err := m.commit(ops, body.Message, name, email)
		if err != nil {
			mockSend(w, 500, msg(err.Error()))
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		mockSend(w, 201, map[string]any{
			"files":  []any{},
			"commit": map[string]any{"sha": sha, "author": map[string]string{"name": name, "email": email, "date": now}, "created": now},
		})

	default:
		mockSend(w, 405, msg("method not allowed"))
	}
}
