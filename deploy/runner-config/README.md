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

`dnsmasq.conf.j2` is the runner DNS allowlist template. The Ansible
role renders it to `/etc/dnsmasq.d/shithubd-runner.conf` from
`shithub_runner_network_allowlist`, binds dnsmasq to the dedicated
Actions Docker bridge, and points step containers at that resolver
with `engine.dns_servers`.

`firewall.sh.j2` is installed as `/usr/local/sbin/shithub-runner-firewall`
and run by `shithub-runner-firewall.service`. It creates the ipset used
by dnsmasq and rejects direct-IP egress from the Actions bridge unless
the destination IP was populated by an allowlisted DNS response. DNS to
the bridge resolver is the only DNS path allowed from step containers.
