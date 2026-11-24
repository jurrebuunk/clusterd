# clusterd

[![CI](https://github.com/jurrebuunk/clusterd/actions/workflows/ci.yml/badge.svg)](https://github.com/jurrebuunk/clusterd/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/jurrebuunk/clusterd?include_prereleases&sort=semver)](https://github.com/jurrebuunk/clusterd/releases)

The platform API for my homelab (`clusterd`), plus `labctl`, its
site-neutral command-line client. Two programs from one repo: the hosts run
`clusterd`, my laptop runs `labctl`.

`clusterd` runs on every hypervisor of the
[hgd1 cluster](https://github.com/jurrebuunk/nix-hgd1-cluster) behind one
highly available endpoint, `https://api.lan.buunk.org`, and is the single
authenticated way to see and manage the platform: status of hosts, VMs, k3s
and Ceph, disks (size, how full, wear), S3 buckets, a read-only view of
metrics and logs, and safe actions (a VM restart
that cordons, drains, restarts and uncordons, one at a time).

**Status:** `v1.0.0` on hgd1 ([PLAN.md](PLAN.md)). Handbook:
[Platform API](https://jurrebuunk.github.io/nix-hgd1-cluster/concepts/api/) ·
[API reference](https://jurrebuunk.github.io/nix-hgd1-cluster/reference/api/) ·
[CLI reference](https://jurrebuunk.github.io/nix-hgd1-cluster/reference/cli/).

## labctl

```sh
nix run github:jurrebuunk/clusterd#labctl -- --help     # or a binary from Releases

labctl auth login                        # asks for a token, checks it, stores it (600)
labctl status                            # hosts, VMs, k3s, Ceph, VIPs, disks, S3
labctl health                            # one verdict per area; exit 5 unless ok
labctl host list                         # a noun, then a verb: vm, node, pod, osd, disk, fs, vip, s3 …
labctl vm restart hgd1-k8w01             # plan, confirm, progress (operator scope)
labctl operation list                    # what ran, for whom, how it ended
labctl s3 user create my-app --out my-app.env
labctl metrics query 'up == 0'
labctl logs --host hgd1-hv01 --unit 'ceph-osd*' --since 2h
labctl -o json status | jq .ceph         # -o table|json|name, anywhere on the line
source <(labctl completion bash)         # zsh and fish too; Nix installs them
```

Sites are contexts (`labctl site use`, `--site`, `LABCTL_SITE`); a site without
config is `hgd1` at `https://api.lan.buunk.org`. Exit codes: 0 ok, 1 API
error, 2 usage, 3 unreachable (check WARP), 4 token missing, invalid or not
allowed, 5 `labctl health` not ok.

## Tokens

```sh
labctl auth token create --name laptop-jurre --scope manage --out ~/.config/labctl/tokens/hgd1
```

prints the line `laptop-jurre manage sha256:…` for the hosts' token file
(`secrets/api-tokens.age` in the cluster repo) and writes the token itself to
the file, never to the terminal. Scopes nest: `read` ⊂ `operator` ⊂ `manage`.

## On a NixOS host

```nix
inputs.clusterd = {
  url = "github:jurrebuunk/clusterd/v1.2.0";
  inputs.nixpkgs.follows = "nixpkgs";
};
# modules = [ clusterd.nixosModules.clusterd ];
services.clusterd = {
  enable = true;
  tokensFile = "/run/agenix/api-tokens";
  kubeconfigFile = "/run/agenix/clusterd-kubeconfig";   # optional
  ceph.keyringFile = "/run/agenix/clusterd-ceph-keyring"; # optional
  manageUnitPrefixes = [ "microvm@" ];                    # polkit: what it may restart
  s3.credentialsFile = "/run/agenix/clusterd-s3-admin";   # optional, bucket usage
  settings = { site = "hgd1"; host = "hgd1-hv01"; listen = [ "10.10.20.11:443" ]; hosts = [ … ]; … };
};
```

All settings are in [`internal/config/config.go`](internal/config/config.go);
the hgd1 wiring is
[`modules/clusterd/`](https://github.com/jurrebuunk/nix-hgd1-cluster/tree/main/modules/clusterd)
in the cluster repo.

## Develop

```sh
nix develop                # go, gopls, goreleaser
go test -race ./...
nix flake check            # packages, module, config check
```

Point `labctl` at one instance with `LABCTL_API=https://10.10.20.12`… or at a
local `clusterd --config dev.toml` without TLS (`http://127.0.0.1:8080`).
Releases: bump `VERSION`, tag `v<VERSION>`, push; see [PLAN.md](PLAN.md#releases).
