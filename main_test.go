package main

// End-to-end: a Go copy of Nuxt Studio's GitLab provider -> shim -> mock Forgejo (real git repo).
//
// studioClient mirrors src/app/src/utils/providers/gitlab.ts from nuxt-studio (1.8.2):
// same URLs, URL encoding, auth headers, request bodies and draft-status handling.
// If you upgrade nuxt-studio, diff that file against this client.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const (
	publicURL  = "https://forgejo.kitcab.co.nz"
	siteOrigin = "https://site.kitcab.co.nz"
)

type testEnv struct {
	shim    *httptest.Server
	forgejo *mockForgejo
	studio  studioClient
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	m := newMockForgejo(t)
	fsrv := httptest.NewServer(m)
	t.Cleanup(fsrv.Close)
	cfg = config{
		forgejoURL:       fsrv.URL,
		forgejoPublicURL: publicURL,
		allowedOrigins:   []string{siteOrigin},
		allowedRepos:     []string{"kit/site"},
		allowedBranches:  []string{"main"},
		allowedPaths:     []string{"content/"},
		committer:        identity{"Content Bot", "content-bot@kitcab.co.nz"},
		maxBodyBytes:     25 << 20,
	}
	shim := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(shim.Close)
	return &testEnv{shim, m, studioClient{
		instanceURL: shim.URL, owner: "kit", repo: "site", branch: "main",
		token: mockToken, authorName: "Kit Editor", authorEmail: "kit@kitcab.co.nz",
	}}
}

// ---------- Studio GitLab provider, ported ----------

const (
	draftCreated  = "created"
	draftUpdated  = "updated"
	draftDeleted  = "deleted"
	draftPristine = "pristine"
)

type draft struct {
	path, content, status, encoding string
}

type studioClient struct {
	instanceURL, owner, repo, branch, rootDir, token, authorName, authorEmail string
}

// noRedirect returns 3xx responses instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// encodeURIComponent matches JavaScript's.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s studioClient) join(p string) string {
	if s.rootDir == "" {
		return p
	}
	return strings.TrimRight(s.rootDir, "/") + "/" + strings.TrimLeft(p, "/")
}

func (s studioClient) api(p string) string {
	return s.instanceURL + "/api/v4/projects/" + encodeURIComponent(s.owner+"/"+s.repo) + p
}

func (s studioClient) setAuth(req *http.Request) {
	if strings.HasPrefix(s.token, "glpat-") {
		req.Header.Set("PRIVATE-TOKEN", s.token)
	} else {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
}

// fetchFile returns the GitLab file object, or nil when the shim says 404 (Studio returns null).
func (s studioClient) fetchFile(t *testing.T, p string) *gitlabFile {
	t.Helper()
	p = s.join(p)
	req, _ := http.NewRequest("GET", s.api("/repository/files/"+encodeURIComponent(p)+"?ref="+s.branch), nil)
	s.setAuth(req)
	status, body := do(t, req)
	if status == 404 {
		return nil
	}
	if status != 200 {
		t.Fatalf("fetchFile %s: %d %s", p, status, body)
	}
	var f gitlabFile
	mustUnmarshal(t, body, &f)
	return &f
}

func (s studioClient) commitFiles(t *testing.T, files []draft, message string) gitlabCommit {
	t.Helper()
	actions := []map[string]string{}
	for _, f := range files {
		if f.status == draftPristine {
			continue
		}
		p := s.join(f.path)
		if f.status == draftDeleted {
			actions = append(actions, map[string]string{"action": "delete", "file_path": p})
			continue
		}
		content, enc := f.content, "text"
		if f.encoding == "base64" {
			enc = "base64"
			if i := strings.Index(content, ";base64,"); strings.HasPrefix(content, "data:") && i != -1 {
				content = content[i+len(";base64,"):]
			}
		}
		action := "update"
		if f.status == draftCreated {
			action = "create"
		}
		actions = append(actions, map[string]string{"action": action, "file_path": p, "content": content, "encoding": enc})
	}
	status, body := s.postCommit(t, map[string]any{
		"branch": s.branch, "commit_message": message, "actions": actions,
		"author_name": s.authorName, "author_email": s.authorEmail,
	})
	if status != 201 {
		t.Fatalf("commitFiles: %d %s", status, body)
	}
	var c gitlabCommit
	mustUnmarshal(t, body, &c)
	return c
}

func (s studioClient) postCommit(t *testing.T, payload any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", s.api("/repository/commits"), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	s.setAuth(req)
	return do(t, req)
}

func (s studioClient) repositoryURL() string { return s.instanceURL + "/" + s.owner + "/" + s.repo }
func (s studioClient) branchURL() string     { return s.repositoryURL() + "/-/tree/" + s.branch }
func (s studioClient) commitURL(sha string) string {
	return s.repositoryURL() + "/-/commit/" + sha
}
func (s studioClient) fileURL(content bool, p string) string {
	dir := "public"
	if content {
		dir = "content"
	}
	return s.repositoryURL() + "/-/blob/" + s.branch + "/" + s.join(dir+"/"+p)
}

// ---------- helpers ----------

func do(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func get(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

func mustUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("bad JSON %q: %v", b, err)
	}
}

func unb64(t *testing.T, s string) string {
	t.Helper()
	// GitLab returns unwrapped base64; Go's decoder would silently skip newlines, so check explicitly.
	if strings.ContainsAny(s, "\r\n") {
		t.Fatalf("base64 content contains newlines: %q", s)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return string(b)
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n got: %v\nwant: %v", what, got, want)
	}
}

// ---------- tests ----------

func TestFetchFileReturnsContentAndBlobSHA(t *testing.T) {
	e := setup(t)
	f := e.studio.fetchFile(t, "content/blog/first.md")
	if f == nil {
		t.Fatal("file not found")
	}
	eq(t, unb64(t, f.Content), "---\ntitle: First\n---\n\nPost\n", "content")
	eq(t, f.BlobID, strings.TrimSpace(e.forgejo.git("rev-parse", "main:content/blog/first.md")), "blob_id")
	eq(t, f.FilePath, "content/blog/first.md", "file_path")
}

func TestFetchFileUnwrapsLongBase64(t *testing.T) {
	e := setup(t)
	long := strings.Repeat("A long line of markdown content. ", 20) + "\n"
	e.studio.commitFiles(t, []draft{{path: "content/long.md", content: long, status: draftCreated}}, "long")
	f := e.studio.fetchFile(t, "content/long.md")
	if f == nil {
		t.Fatal("not found")
	}
	eq(t, unb64(t, f.Content), long, "content")
}

func TestFetchFileMissingAndDirectoryAre404(t *testing.T) {
	e := setup(t)
	if e.studio.fetchFile(t, "content/nope.md") != nil {
		t.Error("missing file returned content")
	}
	if e.studio.fetchFile(t, "content/blog") != nil {
		t.Error("directory returned content")
	}
}

func TestOnePublishIsOneCommit(t *testing.T) {
	e := setup(t)
	git := e.forgejo.git
	before := strings.TrimSpace(git("rev-parse", "main"))
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0x10}

	c := e.studio.commitFiles(t, []draft{
		{path: "content/new.md", content: "# Kia ora ✓ é 日本語\n", status: draftCreated},
		{path: "content/index.md", content: "# Home\n\nUpdated\n", status: draftUpdated},
		{path: "content/blog/first.md", status: draftDeleted},
		{path: "content/img/logo.png", content: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), status: draftCreated, encoding: "base64"},
		{path: "content/untouched.md", content: "x", status: draftPristine},
	}, "Studio: update content")

	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.ID) {
		t.Fatalf("commit id %q", c.ID)
	}
	eq(t, c.ID, strings.TrimSpace(git("rev-parse", "main")), "commit id is branch head")
	eq(t, strings.TrimSpace(git("rev-parse", "main^")), before, "exactly one new commit")
	eq(t, c.WebURL, publicURL+"/kit/site/commit/"+c.ID, "web_url")

	eq(t, git("show", "main:content/new.md"), "# Kia ora ✓ é 日本語\n", "unicode create")
	eq(t, git("show", "main:content/index.md"), "# Home\n\nUpdated\n", "update")
	eq(t, strings.TrimSpace(git("ls-tree", "main", "--", "content/blog/first.md")), "", "deleted")
	eq(t, strings.TrimSpace(git("ls-tree", "main", "--", "content/untouched.md")), "", "pristine skipped")

	f := e.studio.fetchFile(t, "content/img/logo.png")
	if f == nil {
		t.Fatal("png not found")
	}
	eq(t, unb64(t, f.Content), string(png), "binary round-trips byte-exact")

	eq(t, strings.TrimSpace(git("log", "-1", "--format=%an <%ae>|%s", "main")), "Kit Editor <kit@kitcab.co.nz>|Studio: update content", "author and subject")
}

func TestStaleDraftStatusesStillPublish(t *testing.T) {
	e := setup(t)
	e.studio.commitFiles(t, []draft{
		{path: "content/index.md", content: "again\n", status: draftCreated},
		{path: "content/brand-new.md", content: "hi\n", status: draftUpdated},
		{path: "content/already-gone.md", status: draftDeleted},
	}, "stale")
	eq(t, e.forgejo.git("show", "main:content/index.md"), "again\n", "create on existing")
	eq(t, e.forgejo.git("show", "main:content/brand-new.md"), "hi\n", "update on missing")
}

func TestRootDirPathsArePassedThrough(t *testing.T) {
	e := setup(t)
	s := e.studio
	s.rootDir, s.authorName, s.authorEmail = "apps/web", "A", "a@x"
	cfg.allowedPaths = []string{"apps/web/content/"}
	s.commitFiles(t, []draft{{path: "content/x.md", content: "mono\n", status: draftCreated}}, "mono")
	eq(t, e.forgejo.git("show", "main:apps/web/content/x.md"), "mono\n", "committed under rootDir")
	f := s.fetchFile(t, "content/x.md")
	if f == nil {
		t.Fatal("not found")
	}
	eq(t, unb64(t, f.Content), "mono\n", "read under rootDir")
}

func TestAuthHeadersAreTranslated(t *testing.T) {
	e := setup(t)
	u := e.studio.api("/repository/files/content%2Findex.md?ref=main")
	eq(t, get(t, u, map[string]string{"PRIVATE-TOKEN": mockToken}).StatusCode, 200, "PRIVATE-TOKEN")
	eq(t, get(t, u, map[string]string{"Authorization": "Bearer " + mockToken}).StatusCode, 200, "Bearer")
	eq(t, get(t, u, map[string]string{"Authorization": "Bearer wrong"}).StatusCode, 401, "wrong token")
	eq(t, get(t, u, nil).StatusCode, 401, "no token")
	noToken := e.studio
	noToken.token = ""
	req, _ := http.NewRequest("POST", noToken.api("/repository/commits"), strings.NewReader(`{"branch":"main","commit_message":"m","actions":[{"action":"create","file_path":"content/a.md","content":"a"}]}`))
	status, _ := do(t, req)
	eq(t, status, 401, "commit without token")
	for _, h := range e.forgejo.authHeaders() {
		if h != "" && !strings.HasPrefix(h, "token ") {
			t.Errorf("Forgejo got Authorization %q, want \"token ...\"", h)
		}
	}
}

func TestCommitErrorsFromForgejoSurface(t *testing.T) {
	e := setup(t)
	cfg.allowedBranches = []string{"main", "nope"} // allowed by the gate, missing in Forgejo
	status, body := e.studio.postCommit(t, map[string]any{
		"branch": "nope", "commit_message": "m",
		"actions": []map[string]string{{"action": "create", "file_path": "content/a.md", "content": "a"}},
	})
	eq(t, status, 404, "status")
	var r struct{ Message string }
	mustUnmarshal(t, body, &r)
	if !strings.Contains(r.Message, "branch") {
		t.Errorf("message %q should mention the branch", r.Message)
	}
}

func TestAllowedReposAndUnknownRoutes(t *testing.T) {
	e := setup(t)
	auth := map[string]string{"Authorization": "Bearer " + mockToken}
	eq(t, get(t, e.shim.URL+"/api/v4/projects/other%2Frepo/repository/files/a.md?ref=main", auth).StatusCode, 404, "other repo")
	eq(t, get(t, e.shim.URL+"/api/v4/user", auth).StatusCode, 404, "unknown API route")
}

func TestCORS(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("OPTIONS", e.studio.api("/repository/commits"), nil)
	req.Header.Set("Origin", siteOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	eq(t, res.StatusCode, 204, "preflight status")
	eq(t, res.Header.Get("Access-Control-Allow-Origin"), siteOrigin, "allow-origin")
	if !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "authorization") {
		t.Errorf("allow-headers %q", res.Header.Get("Access-Control-Allow-Headers"))
	}
	ok := get(t, e.shim.URL+"/healthz", map[string]string{"Origin": siteOrigin})
	eq(t, ok.Header.Get("Access-Control-Allow-Origin"), siteOrigin, "allowed origin on response")
	evil := get(t, e.shim.URL+"/healthz", map[string]string{"Origin": "https://evil.example"})
	eq(t, evil.Header.Get("Access-Control-Allow-Origin"), "", "other origin")
}

func TestCORSProductionOrigins(t *testing.T) {
	e := setup(t)
	cfg.allowedOrigins = list("https://kitcab.co.nz, https://preview.staging.kitcab.co.nz, http://localhost:8787")
	preflight := func(origin string) *http.Response {
		req, _ := http.NewRequest("OPTIONS", e.studio.api("/repository/commits"), nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	for _, o := range cfg.allowedOrigins {
		res := preflight(o)
		eq(t, res.StatusCode, 204, o+" status")
		eq(t, res.Header.Get("Access-Control-Allow-Origin"), o, o+" allow-origin")
	}
	for _, o := range []string{"https://evil.example", "https://kitcab.co.nz.evil.example", "http://kitcab.co.nz", "https://www.kitcab.co.nz"} {
		eq(t, preflight(o).Header.Get("Access-Control-Allow-Origin"), "", o+" allow-origin")
	}
}

func TestStudioLinksRedirectToForgejo(t *testing.T) {
	e := setup(t)
	s := e.studio
	loc := func(u string) string { return get(t, u, nil).Header.Get("Location") }
	eq(t, loc(s.repositoryURL()), publicURL+"/kit/site", "repository")
	eq(t, loc(s.branchURL()), publicURL+"/kit/site/src/branch/main", "branch")
	eq(t, loc(s.commitURL("abc123")), publicURL+"/kit/site/commit/abc123", "commit")
	eq(t, loc(s.fileURL(true, "blog/a.md")), publicURL+"/kit/site/src/branch/main/content/blog/a.md", "file")
}

// ---------- shim behaviour Studio 1.8.2 doesn't exercise ----------

func TestMoveAction(t *testing.T) {
	e := setup(t)
	status, body := e.studio.postCommit(t, map[string]any{
		"branch": "main", "commit_message": "move",
		"actions": []map[string]string{{"action": "move", "previous_path": "content/index.md", "file_path": "content/home.md"}},
	})
	eq(t, status, 201, "status: "+string(body))
	eq(t, e.forgejo.git("show", "main:content/home.md"), "# Home\n\nHello\n", "moved content kept")
	eq(t, strings.TrimSpace(e.forgejo.git("ls-tree", "main", "--", "content/index.md")), "", "old path removed")
}

func TestNothingToCommit(t *testing.T) {
	e := setup(t)
	status, _ := e.studio.postCommit(t, map[string]any{
		"branch": "main", "commit_message": "m",
		"actions": []map[string]string{{"action": "delete", "file_path": "content/already-gone.md"}},
	})
	eq(t, status, 400, "status")
}

func TestBodyTooLarge(t *testing.T) {
	e := setup(t)
	cfg.maxBodyBytes = 1024
	status, _ := e.studio.postCommit(t, map[string]any{
		"branch": "main", "commit_message": "m",
		"actions": []map[string]string{{"action": "create", "file_path": "big.md", "content": strings.Repeat("x", 2048)}},
	})
	eq(t, status, 413, "status")
}

func TestToBase64(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		content  *string
		encoding string
		want     string
	}{
		{nil, "", ""},
		{str("héllo"), "text", base64.StdEncoding.EncodeToString([]byte("héllo"))},
		{str("aGk="), "base64", "aGk="},
		{str("data:image/png;base64,aGk="), "base64", "aGk="},
		{str("data:text/plain,hi"), "base64", "data:text/plain,hi"},
	}
	for _, c := range cases {
		eq(t, toBase64(c.content, c.encoding), c.want, fmt.Sprintf("toBase64(%v, %q)", c.content, c.encoding))
	}
}

// ---------- content gate: what a leaked token can do ----------

func TestCommitsOutsideContentAreRefusedWhole(t *testing.T) {
	bad := []string{
		"proto41-guard2.txt",
		"content/../x.txt",
		"content/./x.txt",
		"content//x.txt",
		"content/",
		"/content/x.txt",
		`content\x.txt`,
		"content/a/../../x.txt",
		"content/..",
		"contentx/x.txt",
		"Content/x.txt",
		"content/.git/config",
		"content/x\x00.txt",
		"",
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			e := setup(t)
			before := strings.TrimSpace(e.forgejo.git("rev-parse", "main"))
			status, body := e.studio.postCommit(t, map[string]any{
				"branch": "main", "commit_message": "m",
				"actions": []map[string]string{
					{"action": "create", "file_path": "content/ok.md", "content": "ok"},
					{"action": "create", "file_path": p, "content": "x"},
				},
			})
			if status < 400 || status >= 500 {
				t.Errorf("status %d %s, want 4xx", status, body)
			}
			eq(t, strings.TrimSpace(e.forgejo.git("rev-parse", "main")), before, "no new commit")
		})
	}
}

func TestMoveFromOutsideContentIsRefused(t *testing.T) {
	e := setup(t)
	status, _ := e.studio.postCommit(t, map[string]any{
		"branch": "main", "commit_message": "move",
		"actions": []map[string]string{{"action": "move", "previous_path": "nuxt.config.ts", "file_path": "content/config.ts"}},
	})
	eq(t, status, 403, "status")
	eq(t, e.forgejo.git("show", "main:nuxt.config.ts"), "export default {}\n", "source untouched")
}

func TestCommitsToOtherBranchesAreRefused(t *testing.T) {
	e := setup(t)
	cfg.allowedBranches = []string{"prototype/41-studio-commits"} // main itself not allowed
	before := strings.TrimSpace(e.forgejo.git("rev-parse", "main"))
	for _, b := range []string{"main", "prototype/not-allowed", "Prototype/41-studio-commits", "prototype/41-studio-commits/x"} {
		status, body := e.studio.postCommit(t, map[string]any{
			"branch": b, "commit_message": "m",
			"actions": []map[string]string{{"action": "create", "file_path": "content/ok.md", "content": "ok"}},
		})
		eq(t, status, 403, "branch "+b+": "+string(body))
	}
	eq(t, strings.TrimSpace(e.forgejo.git("rev-parse", "main")), before, "no new commit")
}

func TestCommitterIsAlwaysTheBot(t *testing.T) {
	e := setup(t)
	status, body := e.studio.postCommit(t, map[string]any{
		"branch": "main", "commit_message": "m",
		"author_name": "Kit Editor", "author_email": "kit@kitcab.co.nz",
		"committer_name": "Mallory", "committer_email": "mallory@evil.example", // not GitLab fields; must be ignored
		"actions": []map[string]string{{"action": "create", "file_path": "content/prototype/gate-ok.txt", "content": "ok"}},
	})
	eq(t, status, 201, "status: "+string(body))
	eq(t, strings.TrimSpace(e.forgejo.git("log", "-1", "--format=%an <%ae>|%cn <%ce>", "main")),
		"Kit Editor <kit@kitcab.co.nz>|Content Bot <content-bot@kitcab.co.nz>", "author|committer")
}
