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
                  manageUnitPrefixes = [ "microvm@" ];
                  allowReboot = true;
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
          # The polkit rule is valid JavaScript and only allows what it should.
          polkit = pkgs.runCommand "clusterd-polkit-check" { nativeBuildInputs = [ pkgs.nodejs ]; } ''
            cat > rule.js <<'EOF'
            var polkit = { Result: { YES: "yes", NOT_HANDLED: "no" }, rules: [], addRule: function (f) { this.rules.push(f); } };
            ${eval.config.security.polkit.extraConfig}
            function decide(action, user) {
              for (var i = 0; i < polkit.rules.length; i++) {
                var r = polkit.rules[i](action, { user: user });
                if (r !== "no" && r !== undefined && r !== null) { return r; }
              }
              return "no";
            }
            function ask(user, unit, verb) {
              return decide({ id: "org.freedesktop.systemd1.manage-units", lookup: function (k) { return k == "unit" ? unit : verb; } }, user);
            }
            function act(user, id) {
              return decide({ id: id, lookup: function () { return ""; } }, user);
            }
            var cases = [
              ["clusterd", "microvm@hgd1-k8w01.service", "restart", "yes"],
              ["clusterd", "microvm@hgd1-k8w01.service", "stop", "yes"],
              ["clusterd", "microvm@x.service", "enable", "no"],
              ["clusterd", "sshd.service", "restart", "no"],
              ["clusterd", "microvm@x.socket", "restart", "no"],
              ["jurre", "microvm@hgd1-k8w01.service", "restart", "no"],
            ];
            cases.forEach(function (c) {
              var got = ask(c[0], c[1], c[2]);
              if (got !== c[3]) { console.error("polkit", c, "got", got); process.exit(1); }
            });
            [
              ["clusterd", "org.freedesktop.login1.reboot", "yes"],
              ["clusterd", "org.freedesktop.login1.power-off", "no"],
              ["clusterd", "org.freedesktop.login1.set-wall-message", "no"],
              ["jurre", "org.freedesktop.login1.reboot", "no"],
            ].forEach(function (c) {
              var got = act(c[0], c[1]);
              if (got !== c[2]) { console.error("polkit", c, "got", got); process.exit(1); }
            });
            EOF
            node rule.js
            touch $out
          '';
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
