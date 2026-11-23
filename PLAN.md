# Plan

Where this repo is going, so I can pick it up from here. The release pipeline
exists; the code doesn't yet. Work through [Road to v1](#road-to-v1) top to
bottom.

## Goal

A small, stateless REST API on all three hypervisors (`hgd1-hv01..03`) with
one HA endpoint, plus a CLI that uses it for managing and developing. What the
API can *do* is decided action by action later; the foundation is health,
status and a safe, authenticated view of metrics and logs.

## What belongs here

| Part | Notes |
| --- | --- |
| `clusterd` (server) | REST API, JSON, `/v1/...` |
| `labctl` (CLI) | Uses only the API; runs on my laptop; also handy while developing `clusterd`. Site-neutral name: it can talk to more than one site |
| API definition | One OpenAPI file; server and client generated from it |
| NixOS module | `services.clusterd.*` options; the cluster repo only configures it |

Language: **Go** (one static binary, the official Kubernetes client, good CLI
libraries).

Why server and CLI share one repo: they share the API definition, so one
commit changes both sides and a `clusterd` and `labctl` of the same version
always match (the same pattern as Kubernetes and `kubectl`). They are still two
separate programs: the hosts only get `clusterd`, the laptop only `labctl`.

## How it is installed

This repo is a flake that exports:

- `packages.x86_64-linux.clusterd`
- `packages.x86_64-linux.labctl`
- `nixosModules.clusterd`

`nix-hgd1-cluster` takes it as an input and sets the options, the same split
as my other "package + module here, configuration there" repos:

```nix
inputs.clusterd = {
  url = "github:jurrebuunk/clusterd/v1.0.0";  # always a release tag
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Updating = change the tag, `nix flake update clusterd` in the cluster repo,
then a normal host-by-host deploy.

On the laptop, `labctl` comes from the same flake:

- ad hoc: `nix run github:jurrebuunk/clusterd#labctl -- status`
- without Nix: a `labctl` binary from [GitHub Releases](https://github.com/jurrebuunk/clusterd/releases)
- permanently: add `clusterd` as an input of `~/nixos` and
  `inputs.clusterd.packages.x86_64-linux.labctl` to `environment.systemPackages`

## labctl

| | |
| --- | --- |
| Config | `~/.config/labctl/config.toml`: one context per site (API URL, token file), like kubeconfig contexts |
| Token | `~/.config/labctl/token` (mode 600) for the default context |
| Site | `labctl --site hgd1 …` or `labctl use hgd1` |
| Env | `LABCTL_API`, `LABCTL_TOKEN`, `LABCTL_SITE` override the config |

## Runtime design (agreed)

- **Endpoint:** `https://api.lan.buunk.org`, Keepalived VIP `10.10.20.20`
  (VRRP id 21) on the **infra VLAN**, like the S3, k3s API and monitoring
  endpoints. Let's Encrypt certificate through Cloudflare DNS-01.
- **Not public.** From outside the site it's only reachable with WARP.
- **Stateless:** reads systemd, the k3s API, Ceph and the monitoring stores
  live. Nothing to replicate.
- **Auth:** bearer tokens; hashes in an agenix secret in the cluster repo.
  Scopes:

    | Scope | For | Allows |
    | --- | --- | --- |
    | `read` | Dashboards, scripts | Status, metrics, logs |
    | `operator` | The web UI's proxy (`lab-console`) | `read` + a few safe actions |
    | `manage` | The CLI over WARP | Everything |

  Later: OIDC through Keycloak instead of hand-made tokens.
- **Metrics and logs proxy:** only the *query* APIs of VictoriaMetrics and
  VictoriaLogs (`mon.lan.buunk.org`), never write/delete/admin.
- **Narrow privileges:** polkit allows only `microvm@*.service`; its own
  Kubernetes ServiceAccount; a read-only Ceph key.
- **Audit:** every non-read request is logged to the journal, which ends up in
  VictoriaLogs.
- **Nix decides what exists:** the API acts on VMs from the inventory but never
  creates or deletes them.

## Layout

| Path | Holds |
| --- | --- |
| `api/openapi.yaml` | The API definition; server stubs and the client are generated from it (`oapi-codegen`) |
| `cmd/clusterd/` | Server entry point |
| `cmd/labctl/` | CLI entry point |
| `internal/server/`, `internal/auth/`, `internal/proxy/`, `internal/status/`, `internal/actions/` | Server parts |
| `internal/client/` | Generated API client, used by `labctl` |
| `internal/version/` | `Version`, `Commit`, `Date`, set at build time |
| `nix/package.nix`, `nix/module.nix` | Packages and `nixosModules.clusterd` |
| `VERSION` | The version, without `v`; the only place it is written down |

## Road to v1

Two steps. Each ends with a tagged release that runs on the cluster, so
there is always a working version to come back to.

### Step 1: read-only API (`v0.1.0`)

Goal: `labctl status`, `metrics` and `logs` work from my laptop over WARP,
through the VIP, with tokens. Nothing can be changed yet.

**Server**

- [ ] `api/openapi.yaml` with every v1 endpoint from the handbook's
      [API reference](https://jurrebuunk.github.io/nix-hgd1-cluster/reference/api/),
      generated server interface and client.
- [ ] Config file (TOML) set by the NixOS module: listen address, TLS cert
      and key, token file, store URLs, kubeconfig, Ceph keyring.
- [ ] Auth: tokens are 32 random bytes, stored as SHA-256 hashes
      (`name scope hash` per line). Constant-time compare. Scopes nest:
      `manage` ⊃ `operator` ⊃ `read`. Token file re-read on `SIGHUP`.
- [ ] `GET /v1/health`: this process and its local stores answer.
- [ ] `GET /v1/whoami`: token name, scope, and server version.
- [ ] `GET /v1/status`: hosts, VMs (`microvm@*`), k3s nodes, Ceph health,
      VIP owners. Each source has a timeout; a source that fails is reported as
      `unknown`, never fails the whole call.
- [ ] Metrics and logs proxy: allow-list of query paths only, local store
      first, peer fallback, response size and time limits, `?follow=1`
      streaming for logs.
- [ ] Hardening: request timeouts, body limits, JSON errors
      `{"error": …}`, request IDs, structured logs to the journal.
- [ ] Own metrics on `127.0.0.1:9180/metrics` (requests, latency, upstream
      errors) for vmagent.

**labctl**

- [ ] `login`, `use`, `whoami`, `status`, `metrics query|range`, `logs`,
      `token new`, `version` (client and server).
- [ ] Contexts in `~/.config/labctl/config.toml`, env overrides, `--json`,
      the exit codes from the handbook (0–4).

**Packaging**

- [ ] `flake.nix`: `packages.{clusterd,labctl}` (`buildGoModule`,
      version from `VERSION`), `nixosModules.clusterd`, dev shell, `checks`.
- [ ] Module: `services.clusterd` with a fixed system user `clusterd` (the
      polkit rule needs a stable name),
      systemd sandboxing (`ProtectSystem=strict`, no new privileges, only the
      needed `ReadOnlyPaths`), credentials through `LoadCredential`.
- [ ] CI on pushes and PRs: `go vet`, `go test -race`, `nix flake check`.
- [x] Release CI: `.github/workflows/release.yml` + `.goreleaser.yaml`
      (see [Releases](#releases)).

**Cluster repo (`nix-hgd1-cluster`)**

- [ ] `inventory.nix` → `api` (`api.lan.buunk.org`, VIP `10.10.20.20`,
      VRRP 21).
- [ ] `modules/clusterd/`: `default.nix`, `tls.nix` (DNS-01), `vip.nix`
      (Keepalived on `/v1/health`), `firewall.nix`.
- [ ] Secrets: `api-tokens.age`, `clusterd-kubeconfig.age` (read-only
      ServiceAccount: `get`/`list` nodes and pods), Ceph key
      `client.clusterd` with `mon 'allow r'`.
- [ ] DNS override `api.lan.buunk.org` in `hgd1-network`.
- [ ] Scrape `clusterd` metrics in the monitoring config.

**Done when**

- `labctl status` works over WARP from the laptop.
- No token → 401, `read` token on an action path → 403.
- Write and admin paths of VictoriaMetrics/VictoriaLogs are not reachable
  through the API.
- Stopping `clusterd` on the VIP owner moves the VIP; `labctl status` keeps
  working.
- `v0.1.0` is published on GitHub Releases and deployed from that tag.

### Step 2: actions and a stable API (`v1.0.0`)

Goal: the first real action, safe enough for the web UI, and a `/v1` I
promise not to break.

**Actions**

- [ ] Action framework in `internal/actions/`: every action has a scope,
      preflight checks, steps and an audit record.
- [ ] Long actions stream progress as JSON lines in the response; if the
      client disconnects, the action finishes anyway and its result is in the
      audit log.
- [ ] One action per target at a time, across all three instances: a
      Kubernetes `Lease` (`clusterd-vm-<name>`) is the lock. A second request
      gets `409`.
- [ ] `POST /v1/actions/vms/{name}/restart` (scope `operator`): preflight
      (VM exists in the inventory, all other nodes Ready, etcd keeps quorum),
      cordon, drain, restart `microvm@{name}`, wait for a new boot ID and
      Ready, uncordon. Same steps as `scripts/k3s-roll.sh`.
- [ ] `labctl vm restart <vm>`: shows the plan, asks for confirmation
      (`--yes` to skip), shows progress.

**Privileges and audit**

- [ ] Module ships the polkit rule: the `clusterd` user may only
      `restart`/`start`/`stop` `microvm@*.service`.
- [ ] ServiceAccount role widened to: patch nodes, create `pods/eviction`,
      manage `leases` in its own namespace. Nothing else.
- [ ] Audit line for every non-read request: time, token name, scope, action,
      target, result, duration, request ID. Ends up in VictoriaLogs through
      the journal.

**v1 quality**

- [ ] OpenAPI frozen for `/v1`: from here only additive changes; anything
      breaking goes to `/v2`.
- [ ] Contract test: the server is checked against `api/openapi.yaml` in CI.
- [ ] Integration tests against fakes for systemd (D-Bus), Kubernetes
      (`envtest` or fake clientset), Ceph and the stores.
- [ ] Shell completion for `labctl` (bash, zsh, fish).
- [ ] `v1.0.0-rc.1` deployed and used for a week before `v1.0.0`.
- [ ] Handbook: status badges of the API, CLI and their reference pages go
      from *In development* to *Done*.

**Done when**

- A rolling restart of all five k3s VMs with `labctl vm restart`, one by
  one, causes no API downtime.
- Two restarts of the same VM at once: the second gets `409`.
- Every action shows up in `labctl logs --unit clusterd.service`.
- An `operator` token (what `lab-console` will use) can restart a VM, a
  `read` token can't.
- `v1.0.0` is published and deployed.

### After v1

Not part of v1, in rough order: more actions (VM stop/start, host
maintenance mode), OIDC through Keycloak instead of hand-made tokens,
`lab-console` on top of the `operator` scope, more sites as `labctl`
contexts.

## Releases

Versioning is semver; the tag is `v` + the contents of `VERSION`.

1. Bump `VERSION`, commit: `Release v0.1.0`.
2. `git tag v0.1.0 && git push origin main v0.1.0`.
3. [`release.yml`](.github/workflows/release.yml) checks the tag matches
   `VERSION`, runs vet and tests, and GoReleaser publishes a GitHub release
   with:
    - `clusterd_<version>_linux_{amd64,arm64}.tar.gz`
    - `labctl_<version>_{linux,darwin}_{amd64,arm64}.tar.gz`
    - a source archive and `checksums.txt` (SHA-256)
    - a changelog from the commits since the previous tag
    - a signed build-provenance attestation
      (`gh attestation verify <file> -R jurrebuunk/clusterd`)
4. Tags with a suffix (`v1.0.0-rc.1`) become pre-releases.
5. In `nix-hgd1-cluster`, set `inputs.clusterd.url =
   "github:jurrebuunk/clusterd/v0.1.0"`, `nix flake update clusterd`,
   deploy host by host.

The cluster always runs a tagged release, never `main`. Try the packaging
locally with `goreleaser release --snapshot --clean`.
