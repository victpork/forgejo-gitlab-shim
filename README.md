# forgejo-gitlab-shim

Lets **Nuxt Studio** publish to a **Forgejo** repo. Studio is set up as if it were talking to a self-hosted GitLab; this shim answers the two GitLab API calls Studio makes and translates them to Forgejo.

| Studio (GitLab API) | Shim → Forgejo |
|---|---|
| `GET /api/v4/projects/:id/repository/files/:path?ref=` | `GET /api/v1/repos/{owner}/{repo}/contents/{path}?ref=` |
| `POST /api/v4/projects/:id/repository/commits` | `POST /api/v1/repos/{owner}/{repo}/contents` (looks up each file's sha first) |
| Links: `/-/commit/…`, `/-/tree/…`, `/-/blob/…` | 302 redirect to Forgejo's UI |

Single Go file ([main.go](main.go)), standard library only. The Docker image is a static binary on distroless (a few MB, ~10 MB RAM). Written against nuxt-studio 1.8.2.

To run it without Docker: `go build -o shim . && FORGEJO_URL=http://forgejo:3000 ./shim`.

## 1. Forgejo: create a token

Create an access token for the account Studio commits with (User settings → Applications) with the **write:repository** scope. Commits are made by this account, and the commit *author* is set to the Studio editor's name and email.

## 2. Run the shim

Copy this folder next to your Forgejo `compose.yaml`, then add the service from `compose.snippet.yaml` and adjust:

- `FORGEJO_URL` – how the shim reaches Forgejo on the Docker network (e.g. `http://forgejo:3000`)
- `FORGEJO_PUBLIC_URL` – `https://your-forgejo-host.com`
- `ALLOWED_ORIGINS` – the origin of your Nuxt site (Studio calls the shim **from the editor's browser**, so CORS is required)
- `ALLOWED_REPOS` – optional, e.g. `kit/site`

```sh
docker compose up -d --build forgejo-gitlab-shim
```

## 3. Cloudflare Tunnel

Add a public hostname, e.g. `studio-git.com` → `http://forgejo-gitlab-shim:8080`.
If you use Cloudflare Access, don't put this hostname behind it (or bypass `/api/v4/*`): the browser's API calls can't complete an Access login.

## 4. Nuxt config

```ts
// nuxt.config.ts
export default defineNuxtConfig({
  modules: ['@nuxt/content', 'nuxt-studio'],
  studio: {
    repository: {
      provider: 'gitlab',
      owner: 'kit',          // Forgejo user or org
      repo: 'site',
      branch: 'main',
      instanceUrl: 'https://studio-git.com', // the shim, NOT Forgejo
    },
  },
})
```

```sh
# .env on the server running the Nuxt site
NUXT_STUDIO_GIT_GITLAB_TOKEN=<forgejo token from step 1>

# Log in with Google (or use the SSO option) — NOT GitLab OAuth, the shim doesn't fake OAuth
NUXT_STUDIO_AUTH_GOOGLE_CLIENT_ID=...
NUXT_STUDIO_AUTH_GOOGLE_CLIENT_SECRET=...
NUXT_STUDIO_AUTH_GOOGLE_MODERATORS=you@example.com
```

Don't set the `NUXT_STUDIO_AUTH_GITLAB_*` variables.

Studio needs the site deployed with a server (SSR), and your CI must rebuild on pushes to `branch` for published changes to go live.

## Behaviour notes

- **Upserts.** A create for a file that already exists becomes an update, and vice versa; deleting a file that's already gone is skipped. This keeps a stale draft from blocking a publish.
- **The token reaches the browser.** This is how Studio's GitHub and GitLab providers work too. Scope the token to the content repo (a dedicated Forgejo user with access to just that repo is ideal).
- **Upgrades.** If a future Studio release calls other GitLab endpoints, the shim returns `404 … shim does not implement …` and logs it. Pin `nuxt-studio` and check `src/app/src/utils/providers/gitlab.ts` before upgrading.
- Forgejo numeric project IDs and nested groups aren't supported. Studio always sends `owner/repo`, so this doesn't come up.

## Tests

End-to-end tests run a Go port of Studio's GitLab provider against the shim and a mock Forgejo backed by a real git repo. The mock enforces Forgejo's rules: token auth, sha required for update/delete, create fails if the file exists. Each test gets a fresh repo; nothing listens on fixed ports.

```sh
go test ./...   # needs git on PATH
```

The client in [main_test.go](main_test.go) mirrors `src/app/src/utils/providers/gitlab.ts` from nuxt-studio 1.8.2. When upgrading Studio, diff that file against the client.
