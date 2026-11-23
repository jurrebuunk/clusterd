# Plan

Where this repo is going, so I can pick it up from here. Step 1 is done and
running on the hgd1 cluster; next is [step 2](#step-2-actions-and-a-stable-api-v100).

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
| Token | `~/.config/labctl/tokens/<site>` (mode 600) |
| Site | `labctl --site hgd1 …` or `labctl use hgd1` |
| Env | `LABCTL_API`, `LABCTL_TOKEN` (the token itself), `LABCTL_SITE` override the config |

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
| `api/openapi.yaml` | The API definition; handlers and client follow it (contract test in step 2) |
| `cmd/clusterd/` | Server entry point |
| `cmd/labctl/` | CLI entry point |
| `internal/server/`, `internal/auth/`, `internal/config/`, `internal/proxy/`, `internal/status/`, `internal/kube/` | Server parts (`internal/actions/` arrives in step 2) |
| `internal/api/` | JSON types shared by server and client |
| `internal/client/` | API client, used by `labctl` |
| `internal/labctl/` | The CLI's commands |
| `internal/version/` | `Version`, `Commit`, `Date`, set at build time |
| `nix/package.nix`, `nix/module.nix` | Packages and `nixosModules.clusterd` |
| `VERSION` | The version, without `v`; the only place it is written down |

## Road to v1

Two steps. Each ends with a tagged release that runs on the cluster, so
there is always a working version to come back to.

### Step 1: read-only API (`v0.1.0`) ✅

Goal: `labctl status`, `metrics` and `logs` work from my laptop over WARP,
through the VIP, with tokens. Nothing can be changed yet. **Done 2026-11-23.**

**Server**

- [x] `api/openapi.yaml` with every v1 endpoint (validated with
      `openapi-spec-validator`). Handlers and client are written by hand
      against it instead of generated: the proxy endpoints pass bodies through
      untouched, which generated code fits badly. The contract test that keeps
      them in line is part of step 2.
- [x] Config file (TOML) written by the NixOS module; unknown keys are an
      error, `clusterd --check` validates config and tokens (runs as
      `ExecStartPre`).
- [x] Auth: tokens are `lab_` + 32 random bytes, stored as SHA-256 hashes
      (`name scope sha256:hex` per line), compared in constant time against
      every entry. Scopes nest. Tokens and certificate reload on `SIGHUP`
      (`systemctl reload clusterd`, also run by ACME after renewal).
- [x] `GET /v1/health`: 200 while this instance serves. It deliberately does
      not depend on the stores: the proxy falls back to peers, so a dead local
      store must not take the API down with it.
- [x] `GET /v1/whoami`: token name, scope, answering host, server version.
- [x] `GET /v1/node` (new): this host's facts. `/v1/status` asks every peer's
      `/v1/node` with the caller's own token, so instances need no shared
      credential and a peer only answers callers it would answer itself.
- [x] `GET /v1/status`: hosts, VMs (`microvm@*`), k3s nodes, Ceph health, VIP
      owners. Each source has a timeout; failures are reported in place.
- [x] Metrics and logs proxy: allow-listed query paths only, local store
      first, peers on connection errors and 5xx, 64 MiB cap, 30 s timeout,
      `?follow=1` streams VictoriaLogs' tail for at most 1 h, default log
      `limit` 1000. The caller's token is never sent upstream.
- [x] Hardening: header/read timeouts, body limit, JSON errors, request IDs,
      access log to the journal (paths only, never query strings), TLS 1.2+.
- [x] Own metrics on `127.0.0.1:9180/metrics`: requests, latency, auth
      failures, upstream errors and fallbacks, build info.

**labctl**

- [x] `login`, `use`, `whoami`, `status`, `metrics query|range`, `logs`,
      `token new`, `version`.
- [x] Contexts in `~/.config/labctl/config.toml`, tokens in
      `~/.config/labctl/tokens/<site>` (600), env overrides, `--json`, exit
      codes 0–4. `token new --out FILE` writes the token without printing it;
      `login --token-file FILE` reads it.

**Packaging**

- [x] `flake.nix`: `packages.{clusterd,labctl}` (labctl also for macOS),
      `nixosModules.clusterd`, dev shell, `checks` (packages with tests, the
      module's unit, a `--check` of the generated config).
- [x] Module: fixed system user `clusterd`, `CAP_NET_BIND_SERVICE` only,
      `ProtectSystem=strict` and friends, secrets through `LoadCredential`.
- [x] CI on pushes and PRs: gofmt, `go vet`, `go test -race`,
      `nix flake check`.
- [x] Release CI: `.github/workflows/release.yml` + `.goreleaser.yaml`
      (see [Releases](#releases)).

**Cluster repo (`nix-hgd1-cluster`)**

- [x] `inventory.nix` → `api` (`api.lan.buunk.org`, VIP `10.10.20.20`,
      VRRP 21).
- [x] `modules/clusterd/`: `default.nix`, `tls.nix` (DNS-01), `vip.nix`
      (Keepalived on `/v1/health` with full certificate check),
      `firewall.nix`.
- [x] Secrets: `api-tokens.age`, `clusterd-kubeconfig.age` (ServiceAccount
      `clusterd/clusterd`, `get`/`list` on nodes and pods only, declared as a
      k3s manifest in `modules/microvm/guest/k3s/clusterd.nix`),
      `clusterd-ceph-keyring.age` (`client.clusterd`, `mon 'allow r'`).
- [x] DNS override `api.lan.buunk.org` on OPNsense (moves to `hgd1-network`).
- [x] Monitoring: scrapes `clusterd` on every host, probes
      `https://api.lan.buunk.org/v1/health`, pings the VIP.

**Done when**

- [x] `labctl status` works over WARP from the laptop.
- [x] No token → 401, invalid token → 401, `read` token on an action path →
      403 (a `manage` token gets 404: no actions yet).
- [x] Write and admin paths of VictoriaMetrics/VictoriaLogs are not reachable
      through the API (404, also with `..` tricks).
- [x] Stopping `clusterd` on the VIP owner moved the VIP to hv02; a client
      polling 5×/s saw 8 of 116 requests fail (about 2 s), and
      `labctl status` kept working, showing hv01 as unreachable.
- [x] `v0.1.0` is published on GitHub Releases and deployed from that tag.

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
