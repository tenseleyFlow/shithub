{
  description = "shithub Actions default runner image";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          checkoutHelper = pkgs.writeShellApplication {
            name = "shithub-shallow-checkout";
            runtimeInputs = [
              pkgs.git
              pkgs.coreutils
            ];
            text = ''
              set -euo pipefail

              if [ "$#" -ne 3 ]; then
                echo "usage: shithub-shallow-checkout <repo-url> <sha> <dest>" >&2
                exit 2
              fi

              repo_url="$1"
              sha="$2"
              dest="$3"

              mkdir -p "$dest"
              cd "$dest"
              git init
              git remote add origin "$repo_url"
              git fetch --depth=1 origin "$sha"
              git checkout --detach FETCH_HEAD
            '';
          };
          imageRoot = pkgs.buildEnv {
            name = "shithub-runner-nix-root";
            paths = [
              pkgs.bashInteractive
              pkgs.cacert
              pkgs.coreutils
              pkgs.curl
              pkgs.findutils
              pkgs.gcc
              pkgs.git
              pkgs.gnugrep
              pkgs.gnused
              pkgs.gnutar
              pkgs.gzip
              pkgs.gnupg
              pkgs.gnumake
              pkgs.openssh
              pkgs.xz
              checkoutHelper
            ];
            pathsToLink = [ "/bin" "/etc" ];
          };
        in
        {
          runnerImage = pkgs.dockerTools.buildLayeredImage {
            name = "ghcr.io/tenseleyflow/shithub/runner-nix";
            tag = "1.0";
            contents = [ imageRoot ];
            maxLayers = 80;
            config = {
              Cmd = [ "${pkgs.bashInteractive}/bin/bash" ];
              Env = [
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                "GIT_SSL_CAINFO=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                "PATH=/bin:${imageRoot}/bin"
              ];
              WorkingDir = "/workspace";
              Labels = {
                "org.opencontainers.image.title" = "shithub runner-nix";
                "org.opencontainers.image.description" = "Default container image for shithub Actions run steps.";
                "org.opencontainers.image.source" = "https://github.com/tenseleyFlow/shithub";
                "org.opencontainers.image.version" = "1.0";
                "org.opencontainers.image.licenses" = "AGPL-3.0-or-later";
              };
            };
          };

          default = self.packages.${system}.runnerImage;
        });
    };
}
