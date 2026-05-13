# shithub runner image

`flake.nix` builds the default S41d runner container image:

```sh
nix build ./deploy/runner-images#runnerImage
docker load < result
```

The image tag is `ghcr.io/tenseleyflow/shithub/runner-nix:1.0`, matching
`internal/runner/config`'s default. `flake.lock` pins nixpkgs so the
image input set is reviewable and repeatable. The image intentionally
contains only the baseline tools needed for v1 `run:` steps and checkout
plumbing:
`bash`, coreutils, git, curl, CA certificates, gnupg, gcc, gnumake,
archive tools, OpenSSH, and `shithub-shallow-checkout`.

Publishing is handled by `.github/workflows/runner-image.yml`. That
workflow is manual because the GHCR namespace may differ between the
upstream project and self-hosted forks. Leave the image input blank to
publish under the current repository's GHCR namespace, or override it
with `ghcr.io/tenseleyflow/shithub/runner-nix` for the upstream package.
