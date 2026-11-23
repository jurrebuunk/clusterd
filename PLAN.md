# Plan

Where this repo is going, so I can pick it up from here. **v1 is done:**
`v1.0.0` runs on the hgd1 cluster and on my laptop. In progress:
[step 3](#step-3-operate-without-ssh-v110).

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
| `internal/server/`, `internal/auth/`, `internal/config/`, `internal/proxy/`, `internal/status/`, `internal/kube/`, `internal/actions/`, `internal/cmdrun/` | Server parts |
| `internal/kubefake/` | In-memory Kubernetes for tests |
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
promise not to break. **Done: `v1.0.0`, released 2026-11-23.**

**Actions**

- [x] Action engine in `internal/actions/`: preconditions, the lock, steps,
      progress events and audit records; tested against an in-memory
      Kubernetes (`internal/kubefake`).
- [x] Long actions stream progress as JSON lines; they run detached from the
      request, so a client that disconnects doesn't stop them; the journal
      has every step and the result.
- [x] One disruptive action at a time across the cluster: a Kubernetes
      `Lease` (`clusterd/clusterd-disruption`), renewed while running,
      expiring 2 min after a crash. A second request gets `409` naming the
      holder. *Changed from the plan:* one lock for all VMs instead of one per
      VM, because two restarts of different VMs at once could still cost
      quorum or capacity.
- [x] `POST /v1/actions/vms/{name}/restart` (scope `operator`): runs on the
      VM's own host (other instances forward with the caller's token);
      preconditions (VM enabled, all *other* nodes Ready, etcd quorum, no pods
      without a controller) give `412`; cordon, drain with eviction retries on
      disruption budgets, `systemctl restart microvm@…`, wait for a new boot
      ID and Ready, uncordon, `/readyz`. A failed drain uncordons; a VM that
      doesn't come back stays cordoned. `?dry_run=1` returns the plan.
- [x] `labctl vm restart <vm>`: dry run first, confirmation (`--yes` without a
      terminal), progress; `--dry-run` only prints the steps.

**Speed and output** (added during step 2)

- [x] Ceph health cached: the `ceph` CLI takes ~0.5 s, which made every
      `status` slow. A background loop refreshes it every 10 s; concurrent
      refreshes are merged into one; a cache older than 60 s is refreshed
      before answering; `ceph.as_of` says how old it is. `labctl status`
      went from ~0.7 s to under 0.2 s.
- [x] Compact `labctl` output in the style of `kubectl`/`docker`: plain
      tables with upper-case headers, no banners, `Ready,SchedulingDisabled`
      style states, problems as `warning:` lines on stderr,
      `labctl status <section>`, `Client Version`/`Server Version`. `--json`
      is unchanged.

**Status per part, disks and S3** (added for `v1.0.0`)

- [x] `GET /v1/status/{hosts,vms,k3s,ceph,vips,disks,s3}` next to the full
      `/v1/status` (spec `1.1.0`, additive); `labctl status <part>` calls them.
- [x] Disks: every drive's size and how full it is (its OSD, or the
      filesystems on its partitions), role, NVMe wear, temperature, SMART;
      filesystems of hosts and VMs; OSDs. From the SMART/node metrics and
      `ceph osd df` (60 s cache; the Ceph key got `mgr 'allow r'`).
- [x] S3: gateways, users, buckets (objects, size, quota), the data pool's
      room, certificate expiry. Buckets through the RGW admin API with a
      read-only user (`clusterd`, caps `buckets,users,usage,metadata=read`),
      requests signed with SigV4 (checked against AWS's test vectors and a
      real bucket).
- [x] Ceph status reports raw capacity.

**Privileges and audit**

- [x] Module option `manageUnitPrefixes` writes the polkit rule: the
      `clusterd` user may only `restart`/`start`/`stop` units with those
      prefixes (`microvm@`). `nix flake check` runs the rule against allowed
      and refused cases.
- [x] ServiceAccount: read nodes and pods, patch nodes, create evictions,
      manage Leases in its own namespace. Nothing else (checked with
      `kubectl auth can-i`).
- [x] `msg=audit` start and end lines for every action: action, target,
      token, scope, request ID, result, duration.

**v1 quality**

- [x] OpenAPI `1.0.0`; `/v1` frozen: from here only additive changes.
- [x] Contract test: every route and scope matches the spec both ways, and
      real responses (including each stream line) match their schemas: no
      undocumented fields, required ones present.
- [x] Tests with fakes for Kubernetes, `systemctl`, Ceph and the stores.
- [x] Shell completion (`labctl completion bash|zsh|fish`, installed by the
      Nix package), with VM, host and site names.
- [x] `v1.0.0-rc.1` deployed and tested (rolling restart of all VMs).
- [x] `v1.0.0` released the same day on my call instead of after a week; it
      also carries the status parts, disks and S3 below.
- [x] Handbook describes `v1.0.0-rc.1` as built and tested.

**Done when**

- [x] A rolling restart of all five k3s VMs with `labctl vm restart`, one by
      one (agents, then servers, 29–44 s each), with no API errors: 161 of
      161 status requests answered, the Kubernetes API probes never failed.
- [x] Two restarts at once: the second (same VM or another) gets `409`.
- [x] Every action shows up in `labctl logs --unit clusterd audit`.
- [x] An `operator` token restarted a VM (through a forward from hv02 to
      hv03); a `read` token gets `403`.
- [x] `v1.0.0` is published and deployed on the hosts and the laptop.

### Step 3: operate without SSH (`v1.1.0`)

Goal: the routine jobs that still need SSH or `kubectl` go through `labctl`,
so `scripts/vm.sh` and `scripts/k3s-roll.sh` can be deleted.

**VMs and nodes**

- [x] `POST /v1/actions/vms/{name}/stop` (operator): the restart's checks,
      the lock, cordon and drain, then stop `microvm@<name>`. The node stays
      cordoned; a stopped VM starts again at host boot (Nix decides what
      exists).
- [x] `POST /v1/actions/vms/{name}/start` (operator): refuses when it runs;
      start, wait Ready, uncordon.
- [x] `POST /v1/actions/nodes/{name}/cordon` and `/uncordon` (operator), so a
      node left cordoned after a failed restart no longer needs `kubectl`.
- [x] `labctl vm stop|start <vm>`, `labctl node cordon|uncordon <node>`, and
      `labctl vm roll [vm…]`: restart every enabled VM (or the given ones),
      agents first, one at a time, stopping at the first failure; replaces
      `scripts/k3s-roll.sh`.

**Host maintenance**

- [x] `POST /v1/actions/hosts/{name}/maintenance` (manage, runs on that
      host): checks (no other host in maintenance, every node elsewhere
      Ready, etcd keeps quorum, `ceph osd ok-to-stop` for the host's OSDs),
      then the lock, drain and stop the host's VMs (agents first) and
      `ceph osd set-group noout <host>`. `?reboot=1` reboots the host after
      the answer (polkit allows the `clusterd` user only that).
- [x] `POST /v1/actions/hosts/{name}/resume` (manage): start the VMs that
      don't run, wait Ready, uncordon, `ceph osd unset-group noout <host>`,
      wait until all PGs are clean.
- [x] Maintenance is Ceph's per-host `noout` flag, so it survives the reboot
      and `labctl status host` shows `Up,Maintenance` without extra state.
- [x] The Ceph key may run `osd set-group` and `osd unset-group`, nothing
      else that writes.
- [x] `labctl host maintenance <host> [--reboot]`, `labctl host resume <host>`.

**Health**

- [x] `GET /v1/checks`: one verdict over everything (hosts, failed units, VMs,
      nodes, Ceph, VIPs, disks, S3, tunnel, probes and targets, clock),
      each area `ok`, `warn` or `fail` with the reason.
- [x] `labctl health`: the checks as a table; exit code 5 when anything isn't
      `ok`, so a timer can use it as the heartbeat.

**Smaller**

- [x] `labctl logs --unit 'ceph-osd*'` (prefix) and unit names with dots.
- [x] Completion for the new commands.

**Cluster repo and docs**

- [x] Pin `v1.1.0`; polkit reboot rule on.
- [x] The Ceph cap (`osd set-group`/`unset-group`).
- [x] Delete `scripts/vm.sh` and `scripts/k3s-roll.sh`; the handbook uses
      `labctl vm stop|start|roll`, `labctl node uncordon`,
      `labctl host maintenance|resume` and `labctl health`.

**Done when**

- [x] A VM stopped and started again through `labctl`, with the node drained
      before and Ready after.
- [x] `labctl vm roll` over all five VMs without API errors.
- [ ] *(not yet tested live)* A host taken into maintenance (VMs stopped, `noout` on that host),
      rebooted, and resumed to `HEALTH_OK` with all nodes Ready, only
      through `labctl`.
- [x] `labctl health` exits 0 on a healthy site and 5 with a broken part.
- [x] `v1.1.0` released and deployed; the scripts are gone.

### After v1

Not part of v1, in rough order: OIDC through Keycloak instead of hand-made tokens,
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
