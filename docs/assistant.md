# AI assistant (in development)

The assistant is implemented in AppManagement and opens from the **AI assistant** button beside **+** in the CasaOS Apps panel. The chat panel has conversation topics on the left, a new-chat action, a model picker, and a permission selector beside the composer. Provider connections are managed through **CasaOS Settings → AI settings → Manage**.

## Providers

Provider setup follows [OpenCode's connect-then-select flow](https://opencode.ai/docs/providers/), adapted to Vue and the existing Go backend. In **AI settings**, choose **DeepSeek** or **OpenCode Go**, enter an API key and click **Connect**. A short real completion with the runtime tool definitions is tested before saving; this validates tool-schema acceptance without executing a tool. The searchable model picker in the chat header lists models from connected providers, grouped by provider, with display names, context limits and reasoning capability. Model changes apply to new conversations.

Credentials are saved separately for each provider and CasaOS user in a private `assistant/<user ID>.json` file next to the configured apps directory (normally `/var/lib/casaos/assistant`). Files use mode 0600. Keys are never returned by the settings API or stored in browser storage. Existing single-provider files migrate on save. Switching models or returning to a connected provider preserves its key. **Disconnect** removes only that provider; a running conversation retains its credential snapshot until it ends or the process exits.

Model metadata comes from [Models.dev](https://models.dev), fetched server-side from `https://models.dev/api.json`, at most once per 24 hours per process (including failed attempts). Fetches have an eight-second timeout and a 16 MiB bound. No credentials are sent to the catalog. A bundled subset in `internal/assistant/models-dev.json` supports offline startup; the source's MIT license is retained alongside it. The UI reports whether it is using the bundled or refreshed catalog. To update the bundled snapshot, retain the `deepseek` and `opencode-go` entries from the public JSON endpoint.

Supported routing uses Chat Completions with tool calls:

- DeepSeek: `https://api.deepseek.com/chat/completions`.
- OpenCode Go: `https://opencode.ai/zen/go/v1/chat/completions`. Requests include the CasaOS client user agent and a stable `x-opencode-session` conversation ID.

The catalog is filtered to nondeprecated tool-calling models compatible with this adapter. Models requiring Responses, Anthropic, alternate endpoints or unsupported reasoning formats are excluded. Endpoint addresses remain trusted backend configuration; remote metadata cannot redirect provider credentials. There is no silent fallback to another provider. Reasoning content is retained when replaying tool-call context.

Provider contracts: [DeepSeek API guide](https://api-docs.deepseek.com/) and [OpenCode Go endpoints](https://opencode.ai/docs/go/). Catalog membership does not guarantee account access. Connection verification and live inspection require the owner's API key.

For app passwords, indexer keys and Plex claim tokens, use **Add private credential** (the key icon beside the composer). Choose a short name such as `usenet_password`; the model uses `$secret:usenet_password` in credential fields. The server resolves that reference immediately before tool validation. Its value is never added to provider messages or browser storage. References are rejected in URLs, paths and other noncredential fields. App credentials remain in conversation memory; a server restart requires re-entry. Native app configuration necessarily stores the credentials in that app’s own settings.

## Harness and permissions

All assistant routes require a valid CasaOS JWT, including requests from loopback. Assistant schema-validation errors omit submitted values from responses and request logs, including invalid credential requests. Sessions, topic lists, decisions and settings are scoped to the JWT's user ID. A caller-supplied `user_id` header cannot replace JWT validation.

- **Inspect only:** observation tools can run; write tools cannot execute.
- **Review changes:** observations run automatically; each validated change pauses with a concrete summary and a single-use approval ID. Declining returns that decision to the model.
- **Automatic changes:** the tools may install, configure and restart apps for the task without individual approvals.

The model has no shell or Docker exec tool. Tools are explicitly registered, arguments have bounded sizes, and unknown tools are rejected. A run is limited to 24 model steps, eight calls per completion and 20 minutes; provider calls time out after two minutes. Stop cancels the active context. Docker operations already dispatched may have changed state, so cancellation does not imply rollback. Configuration apply failures attempt to restore the original Compose file and running settings with an independent recovery timeout.

A conversation shows model explanations, tool actions/results and proposed changes. Logs and pages are treated as untrusted observations in the system instructions. Known credential patterns and known app environment secrets are redacted, but arbitrary private log text cannot be guaranteed secret-free. Output is rendered as text in the UI, not HTML.

## Tools currently implemented

| Tool | Behavior |
| --- | --- |
| `list_apps` | Lists Docker container names, Compose identities, images, state and published ports. |
| `inspect_app` | Reads a CasaOS Compose app's services, environment, mounts, ports and container state. |
| `app_logs` | Reads bounded tails from the app's containers, including multiplexed Docker logs. |
| `check_app` | Requests the root page at an observed published HTTP port and reports status/page text. No arbitrary URLs, proxy, redirects, scripts or interactive browser actions. |
| `install_stack` | Installs up to 12 services in a new Compose project, with explicit image tags, TCP mappings and host mounts under `/DATA`. Does not replace existing app directories. |
| `configure_service` | Merges environment values, optionally changes the tagged image or port mappings, merges `/DATA` bind mounts by container target while preserving unspecified mounts, and adds persistent shared networks between separately installed apps. Mount changes do not migrate existing data. Checks for stale settings, takes the shared app-operation lock, saves a private `.assistant.bak`, applies synchronously, and attempts recovery on failure. |
| `retry_app` | Pulls images and reconciles the saved Compose configuration, including a partially failed installation. Retains settings and data. |
| `restart_app` | Restarts all containers in the selected app under the shared app-operation lock. |
| `media_read` | Uses Sonarr, NZBGet or Plex APIs to read status and relevant settings, clients, indexers, root folders or libraries. Privately reads the app's fixed `/config` credential file through Docker's read-only archive API. |
| `connect_sonarr_nzbget` | Verifies a shared Docker network and matching `/data` bind mounts, tests the NZBGet client through Sonarr, and creates/updates a named download client using the existing credentials privately. Checks container/config identities and Sonarr client settings again before applying. |
| `create_app_directory` | Creates missing directories inside validated persistent media mounts using the Docker archive API and the service PUID/PGID; preserves existing directories and permissions. |
| `configure_nzbget` | Merges allowlisted news-server, category and download-path settings. Tests changed active news servers, saves a private backup, reloads, verifies and attempts to restore settings on failure. |
| `add_sonarr_root_folder` | Registers an existing writable media directory and verifies the saved root folder. Repeated requests reuse the existing root. |
| `configure_sonarr_indexer` | Tests and creates/updates a named Newznab indexer through Sonarr, retaining a private settings backup. |
| `npm_inspect` | Reads the connected Nginx Proxy Manager, certificate metadata, eligible wildcard domains, hosts and Access Lists. Secrets and login identity are excluded from model observations. |
| `publish_app` | Publishes an observed web service under the configured wildcard domain using HTTPS, WebSockets and the selected access policy. Connects a persistent shared Docker network if necessary, verifies TLS/HTTP and updates the CasaOS card. |
| `check_app_url` | Checks a saved publication through its associated NPM, with DNS resolution, TLS trust/wildcard validation and HTTP status. Does not follow redirects or accept arbitrary URLs. |
| `add_plex_library` | Creates a named TV or movie library for an existing persistent media path and verifies it through Plex. Repeated matching requests reuse the library. |

Native API adapters target LinuxServer-style `/config` layouts and published HTTP ports. Sonarr uses the [v3 API contract](https://sonarr.tv/docs/api/); NZBGet uses its documented [JSON-RPC API](https://nzbget.com/documentation/api/). Create the NZBGet category before connecting it to Sonarr. Directory creation precedes Sonarr root folders and Plex libraries. Plex `media_read` resource `setup` reports the actual claimed state and whether libraries are accessible. For an unclaimed LinuxServer Plex instance, the owner obtains a short-lived token at [plex.tv/claim](https://plex.tv/claim), enters it with **Add private credential** as `plex_claim`, and the assistant sets `PLEX_CLAIM=$secret:plex_claim` through `configure_service`. The private credential form links to the claim page. Tokens expire within four minutes, as described by [LinuxServer Plex documentation](https://docs.linuxserver.io/images/docker-plex/). The assistant checks claim state again and asks the owner to finish Plex Web setup if needed. It does not replace an existing claimed account.

For separately installed apps, set `configure_service.shared_network` to the same `casaos-ai-` prefixed name on each participating service. The assistant creates or reuses a labeled internal Docker bridge, preserves the original networks, and adds unique `<app>-<service>` DNS aliases. The network is stored as external in each Compose file, so recreation retains connectivity. Services with an explicit `network_mode` (including the built-in `bridge`, `host`, `none`, or container sharing) must first be converted to Compose networks in app settings. This tool does not silently replace their network mode. Unrelated networks cannot be joined. Shared networks are retained when an app operation fails so other connected apps remain intact.

NZBGet configuration checks both the saved file and active runtime options after reload. Changing `MainDir` pins an inherited `QueueDir` to its original resolved path, preserving queue/history storage. Directory changes are refused while downloads are queued or processing. Reapplying identical settings checks their active values without reloading NZBGet.

## Nginx Proxy Manager

In **AI settings → Nginx Proxy Manager → Manage NPM**, select the automatically discovered Compose application and enter its NPM login in the private form. **Check certificates** reads the installed certificate inventory. Choose a wildcard domain and explicitly choose **Public** or an existing Access List, then save. A single eligible domain is selected automatically. Credentials are private per CasaOS user under `assistant/npm/<user ID>.json` (0600); the UI never receives the saved password and the model never receives the login or API tokens. New conversations and server restarts reuse the saved connection. Disconnect removes the credentials and invalidates pending publication approvals while retaining existing proxy hosts and recovery records.

The adapter is verified with `jc21/nginx-proxy-manager:2.15.1` and its native `/api` contract. NPM must be a running, unscaled CasaOS Compose service with published management port 81 and HTTPS port 443. Addresses and host port mappings are discovered from Docker, including after container recreation. Discovery and connection validation resolve immutable image IDs through container/image inspection, so NPM remains selectable after CasaOS updates pin its running image. Login must support issuing an API token from the saved credentials; interactive authentication challenges are reported without bypassing them. No shell access, database editing or certificate/private-key download endpoints are used.

**Wildcard certificates are mandatory.** The backend derives eligible suffixes from unexpired wildcard entries and chooses the latest expiry, then lowest certificate ID on a tie. `*.example.com` permits `app.example.com`, not `example.com` or `app.home.example.com`. The agent supplies only one hostname label (by default, the app name with underscores changed to hyphens). The suffix and access policy come from the owner's saved settings. Setup and publication are blocked without suitable wildcard coverage; app installation and other inspection remain available. Certificate issuance, renewal and DNS configuration remain managed outside this integration. There is no HTTP or single-host-certificate fallback.

Publication rechecks certificates, access policy, Compose fingerprints and existing hosts before applying. Routing first reuses an unambiguous persistent service alias, then an existing published IPv4 TCP port. Host-port routing matches the selected container port against both saved Compose and Docker; wildcard host bindings use a gateway of NPM’s persistent non-internal bridge, and explicit non-loopback bindings use their host IP. This supports NPM with `network_mode: bridge` without changing networks or restarting either container. Loopback-only bindings and ephemeral host ports are not used. Only when neither route exists and both services allow Compose networks does publication add an assistant-managed shared bridge; that can restart both services. The proposal shows the host destination and execution rejects a changed destination. `npm_inspect` includes existing hosts’ forward scheme, host and port. The upstream uses HTTP; the published host uses HTTPS, HTTP-to-HTTPS redirection and WebSockets.

A durable ownership marker is saved before host creation and sent in NPM metadata. Repeated requests recover unknown outcomes without duplicate hosts, and the adapter refuses to take over unrelated routes or overwrite advanced/custom-location configuration. Settings and connection changes invalidate prepared proposals. An API write, network change or card update can fail independently: partial outcomes are reported and retained for inspection/retry, rather than claiming transactional rollback.

The final check connects directly to the observed local NPM HTTPS port using the requested Host/SNI. It validates system trust, hostname, certificate validity and the served wildcard SAN, and requires DNS resolution. It reports HTTP authentication/access-policy responses distinctly from upstream errors. DNS resolution alone does not establish that external clients reach this NPM; the result explicitly identifies the local checking perspective. Only verified results update the card's existing hostname/scheme/port fields, preserving its path and saving a private Compose backup. Successful tool events include a safe HTTPS link and refresh the app list. No container restart is needed for the card metadata itself.

For the disposable NPM integration test:

```sh
CASAOS_ASSISTANT_NPM_DOCKER_TEST=1 go test ./service -run '^TestAssistantNPMDockerPublication$' -count=1 -timeout 7m
```

This uses a temporary CA and local DNS fixture, a real NPM container and a Python HTTP/WebSocket service. It verifies certificate discovery, mandatory wildcard coverage, network creation, HTTPS and WebSockets, Access List authentication, idempotency/unknown-outcome recovery, card metadata, container recreation, removed certificates and disconnected approvals. Its CA is supplied only to the test; production checks use system trust. All fixture containers and volumes are removed afterward.

## Conversation lifecycle

Topics, transcripts and model context are stored in private `assistant/history/<conversation ID>.json` files (0600, in a 0700 directory). Provider keys remain in the separate settings store; app secrets and executable plans are never saved with history. The browser stores only the selected conversation ID in sessionStorage, scoped to the account username. Closing and reopening the panel resumes polling. New chat preserves earlier conversations, and users can delete inactive conversations from the sidebar. History is retained until deletion, with limits of 32 conversations per user and 128 overall.

A checkpoint is saved before external work. On restart, unfinished tool calls receive an explicit unknown-outcome result, pending approvals are invalidated, and the conversation becomes ready for a user reply. No action runs automatically during recovery. The assistant must inspect current app state before proposing further changes. Completed conversations retain their context and model selection; replying obtains the saved key for the same provider. Switching providers never silently redirects old conversations. Private app credentials must be added again after a restart.

Storage failures stop new work and appear as an error in the conversation. This is an operation record, not a transaction across Docker and the history filesystem: an operation may finish before its result is checkpointed. Recovery therefore reports uncertainty instead of claiming rollback or repeating the operation.

## Validation

```sh
go generate ./...
go test -race ./internal/assistant
go test ./... -run '^$'
go test ./service ./route/...
go test ./pkg/docker -run '^TestPullStreamDetectsDaemonErrors$'
go build -o /tmp/casaos-app-management-dev .
CASAOS_ASSISTANT_DOCKER_TEST=1 go test ./service -run '^TestAssistant' -count=1 -timeout 7m
CASAOS_ASSISTANT_MEDIA_DOCKER_TEST=1 go test ./service -run '^TestAssistantMedia' -count=1 -timeout 8m
CASAOS_ASSISTANT_PLEX_DOCKER_TEST=1 go test ./service -run '^TestAssistantPlexDockerLibrary$' -count=1 -timeout 7m
CASAOS_ASSISTANT_HTTP_DOCKER_TEST=1 go test ./route -run '^TestAssistantAuthenticatedDockerWorkflow$' -count=1 -timeout 6m
CASAOS_ASSISTANT_NETWORK_DOCKER_TEST=1 go test ./service -run '^TestAssistant.*Network' -count=1 -timeout 6m
```

The opt-in Docker tests use uniquely named disposable Compose projects and remove them afterward. The first covers installation, HTTP checks, log reads, environment changes (including literal dollar signs), backup creation, restart and retrying an app with missing containers. The media test uses real Sonarr/NZBGet containers plus a private NNTP/Newznab fixture to verify directories, categories, news-server authentication, root folders, indexers and download-client linking. The Plex test verifies the unclaimed-account guidance and creates/reads back a library in a disposable Plex container. The network test connects two separate apps, recreates their containers, and verifies DNS/HTTP access, retained default networks and a sentinel file in persistent storage. The authenticated HTTP test exercises signed CasaOS JWTs, cross-user rejection, provider request/response handling, review approval, a real Docker configuration change, HTTP verification and conversation continuation after restarting the manager/router. Its provider and JWKS endpoints are local fixtures. These tests do not exercise paid Usenet downloads, real indexers or third-party accounts.

In the sibling UI repository run `pnpm exec vitest run` and `pnpm run build`. Component tests mock the API; the local provider preview below uses the real backend. Also run `node --test dev/assistant-preview/proxy-auth.check.mjs` for the development proxy guards.

## Local provider verification

The local preview uses the actual JWT middleware, settings store, provider adapter and conversation manager. Its separate runtime filters all mutation tools and rejects direct attempts to execute them. The normal CasaOS runtime continues to support review and automatic changes.

From AppManagement:

```sh
go build -o /tmp/casaos-assistant-preview-server ./cmd/assistant-preview
/tmp/casaos-assistant-preview-server
```

From the sibling UI repository:

```sh
pnpm run dev:assistant
```

Open `http://127.0.0.1:5189/` directly. The dev proxy reads the expiring JWT from the private `startup.json` file and adds it server-side to requests for the loopback API on port 5190. No startup link or browser token is required. The proxy requires the exact loopback Host, same-origin browser metadata and a non-simple preview header; cross-site requests and preflights are rejected. Production JWT authentication remains enabled. If using a custom backend `--state`, set `CASA_PREVIEW_STARTUP` to its `startup.json` path when starting the frontend. Restart the backend if its 12-hour local JWT expires.

Choose a provider and enter its key in **AI settings**, then send an inspection request. Provider keys and conversation history are stored privately under the printed state directory; use **Disconnect** to delete the provider credential. Docker container listing works immediately. For app settings/logs/native API checks, pass `--apps-root` with an existing CasaOS Compose directory. The preview uses the current Docker context when `DOCKER_HOST` is unset. Its state directory is locked to prevent two preview processes from writing the same history. Stop the API and UI processes with Ctrl+C when finished.

## Work still required for the full objective

- Validate actual Plex account claiming and third-party account setup with the owner’s credentials.
- Extend web checks if JavaScript rendering and authenticated browser navigation are needed; current checks inspect HTTP responses only.
- Exercise real provider tool calls with configured credentials and verify a complete multi-app workflow through the authenticated CasaOS interface.

These are local development changes. Building does not deploy them to a CasaOS server. Install AppManagement before the UI when deploying the matching APIs and interface.
