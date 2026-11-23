# clusterd

The platform API for my homelab (`clusterd`), plus `labctl`, its
site-neutral command-line client. Two programs from one repo: the hosts run
`clusterd`, my laptop runs `labctl`.
`clusterd` runs on every hypervisor of the
[hgd1 cluster](https://github.com/jurrebuunk/nix-hgd1-cluster) behind one
highly available endpoint, `https://api.lan.buunk.org`, and is the single
authenticated way to see and manage the platform.

**Status:** planning. Start with [PLAN.md](PLAN.md).

Handbook: <https://jurrebuunk.github.io/nix-hgd1-cluster/concepts/api/>
