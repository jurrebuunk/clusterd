{
  description = "clusterd, the platform API for my homelab, and labctl, its CLI";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      lib = nixpkgs.lib;
      linux = [ "x86_64-linux" "aarch64-linux" ];
      systems = linux ++ [ "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs:
        let
          build = pkgs.callPackage ./nix/package.nix { inherit self; };
        in
        {
          labctl = build "labctl";
          default = build "labctl";
        }
        # The server only makes sense on Linux.
        // lib.optionalAttrs pkgs.stdenv.isLinux { clusterd = build "clusterd"; });

      nixosModules.clusterd = import ./nix/module.nix self;
      nixosModules.default = self.nixosModules.clusterd;

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.goreleaser ];
        };
      });

      checks = lib.genAttrs linux (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          # Evaluate the module on a minimal system and build its unit file.
          eval = lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.clusterd
              {
                boot.loader.grub.enable = false;
                fileSystems."/" = { device = "none"; fsType = "tmpfs"; };
                system.stateVersion = "26.05";
                services.clusterd = {
                  enable = true;
                  tokensFile = "/run/secrets/api-tokens";
                  settings = {
                    site = "test";
                    host = "test";
                    listen = [ "127.0.0.1:8443" ];
                    hosts = [{
                      name = "test";
                      api = "https://127.0.0.1:8443";
                      metrics = "http://127.0.0.1:8428";
                      logs = "http://127.0.0.1:9428";
                    }];
                  };
                };
              }
            ];
          };
        in
        {
          inherit (self.packages.${system}) clusterd labctl;
          module = eval.config.systemd.units."clusterd.service".unit;
          config = pkgs.runCommand "clusterd-config-check" { } ''
            touch tokens
            sed 's#/run/credentials/clusterd.service/tokens#'"$PWD"'/tokens#' \
              ${eval.config.environment.etc."clusterd/config.toml".source} > config.toml
            ${self.packages.${system}.clusterd}/bin/clusterd --config config.toml --check
            touch $out
          '';
        });
    };
}
