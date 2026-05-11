# Runner config assets

`seccomp.json` is a pinned copy of Docker/Moby's default seccomp
profile. It is copied to `/etc/shithubd-runner/seccomp.json` by the
`shithubd-runner` Ansible role and passed to each step container via:

```sh
--security-opt=seccomp=/etc/shithubd-runner/seccomp.json
```

Source: `moby/moby` commit
`7d169a7f0ccd8f79edb6ad02ba20025cb487b217`,
`vendor/github.com/moby/profiles/seccomp/default.json`.

Update this file deliberately when changing Docker daemon versions or
runner syscall posture.

`dnsmasq.conf.j2` is the optional runner DNS allowlist template. The
Ansible role renders it to `/etc/shithubd-runner/dnsmasq.conf` from
`shithub_runner_network_allowlist`; operators can run dnsmasq bound to
their Actions Docker bridge and point step containers at it with
`engine.dns_servers`.

The dnsmasq template intentionally has no default upstream resolver, so
names outside the allowlist fail resolution. DNS allowlisting alone does
not block direct-IP egress or a workflow that brings its own resolver;
pair it with host firewall rules on the runner bridge for a deny-by-
default network boundary.
