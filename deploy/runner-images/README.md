# shithub runner image

`flake.nix` builds the default S41d runner container image:

```sh
nix build ./deploy/runner-images#runnerImage
docker load < result
```

The image tag is `ghcr.io/tenseleyflow/shithub/runner-nix:1.1`, matching
`internal/runner/config`'s default. `flake.lock` pins nixpkgs so the
image input set is reviewable and repeatable.

The image intentionally stays a small hosted Linux baseline, not a
repo-specific build environment. It includes the tools needed for v1
`run:` steps, checkout plumbing, and native dogfood projects:
`bash`, coreutils, git, curl, CA certificates, gnupg, gcc, gfortran,
gnumake, gawk, diffutils, file, procps, util-linux, which, archive
tools, OpenSSH, Python 3.12 with pip/virtualenv, and
`shithub-shallow-checkout`.

Language version selection belongs in first-party setup actions where
shithub supports them. For example, S41n-2 owns the
`actions/setup-python@v5` compatibility shim; this image only provides
the pinned interpreter/tooling that shim can expose.

Publishing is handled by `.github/workflows/runner-image.yml`. That
workflow is manual because the GHCR namespace may differ between the
upstream project and self-hosted forks. Leave the image input blank to
publish under the current repository's GHCR namespace, or override it
with `ghcr.io/tenseleyflow/shithub/runner-nix` for the upstream package.
