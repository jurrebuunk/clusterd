# clusterd

The platform API for my homelab (`clusterd`), plus `labctl`, its
site-neutral command-line client. Two programs from one repo: the hosts run
`clusterd`, my laptop runs `labctl`.
`clusterd` runs on every hypervisor of the
[hgd1 cluster](https://github.com/jurrebuunk/nix-hgd1-cluster) behind one
highly available endpoint, `https://api.lan.buunk.org`, and is the single
authenticated way to see and manage the platform.

[![Release](https://img.shields.io/github/v/release/jurrebuunk/clusterd?include_prereleases&sort=semver)](https://github.com/jurrebuunk/clusterd/releases)
[![Release workflow](https://github.com/jurrebuunk/clusterd/actions/workflows/release.yml/badge.svg)](https://github.com/jurrebuunk/clusterd/actions/workflows/release.yml)

**Status:** planning; the road to v1 is in [PLAN.md](PLAN.md). Releases
(binaries for `clusterd` and `labctl`) are published on
[GitHub Releases](https://github.com/jurrebuunk/clusterd/releases) from
version tags.

Handbook: <https://jurrebuunk.github.io/nix-hgd1-cluster/concepts/api/>
