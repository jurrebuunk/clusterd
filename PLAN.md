# Plan

Where this repo is going, so I can pick it up from here. Nothing is built yet.

## Goal

A small, stateless REST API on all three hypervisors (`hgd1-hv01..03`) with
one HA endpoint, plus a CLI that uses it for managing and developing. What the
API can *do* is decided action by action later; the foundation is health,
status and a safe, authenticated view of metrics and logs.

## What belongs here

| Part | Notes |
| --- | --- |
| `clusterd` (server) | REST API, JSON, `/v1/...` |
| `hgd1` (CLI) | Uses only the API; also handy while developing `clusterd` |
| API definition | One OpenAPI file; server and client generated from it |
| NixOS module | `services.clusterd.*` options; the cluster repo only configures it |

Language: **Go** (one static binary, the official Kubernetes client, good CLI
libraries).

## How it is installed

This repo is a flake that exports:

- `packages.x86_64-linux.clusterd`
- `packages.x86_64-linux.hgd1`
- `nixosModules.clusterd`

`nix-hgd1-cluster` takes it as an input and sets the options, the same split
as my other "package + module here, configuration there" repos:

```nix
inputs.clusterd = {
  url = "github:jurrebuunk/clusterd";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Updating = `nix flake update clusterd` in the cluster repo, then a normal
host-by-host deploy.

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

## First version

1. `GET /v1/health` (Keepalived uses it), `/v1/whoami`, `/v1/status`.
2. Metrics proxy (`query`, `query_range`, `series`, `labels`) and logs proxy
   (`query`, `hits`, `streams`, follow).
3. CLI: `login`, `whoami`, `status`, `metrics`, `logs`, `token new`.
4. NixOS module + flake input in the cluster repo; deploy.
5. First action: `vm restart` (drain → restart → Ready → uncordon).

## Later

CI and the directory layout are not set up yet on purpose; decide them when
coding starts. The handbook already documents the intended API and CLI:
[Platform API](https://jurrebuunk.github.io/nix-hgd1-cluster/concepts/api/),
[API reference](https://jurrebuunk.github.io/nix-hgd1-cluster/reference/api/),
[CLI reference](https://jurrebuunk.github.io/nix-hgd1-cluster/reference/cli/).
