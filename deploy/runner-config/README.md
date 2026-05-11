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
