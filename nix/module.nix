# services.clusterd: the platform API as a hardened systemd service. The
# cluster repo sets the options; nothing site-specific lives here.
self:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.clusterd;
  toml = pkgs.formats.toml { };
  credDir = "/run/credentials/clusterd.service";

  # Secrets reach the service as systemd credentials, never through the store.
  credentials =
    { tokens = cfg.tokensFile; }
    // lib.optionalAttrs (cfg.kubeconfigFile != null) { kubeconfig = cfg.kubeconfigFile; }
    // lib.optionalAttrs (cfg.ceph.keyringFile != null) { ceph-keyring = cfg.ceph.keyringFile; }
    // lib.optionalAttrs (cfg.s3.credentialsFile != null) { s3-admin = cfg.s3.credentialsFile; };

  settings = lib.recursiveUpdate cfg.settings (
    {
      tokens_file = "${credDir}/tokens";
    }
    // lib.optionalAttrs (cfg.kubeconfigFile != null) {
      kubernetes.kubeconfig = "${credDir}/kubeconfig";
    }
    // lib.optionalAttrs (cfg.ceph.keyringFile != null) {
      ceph = {
        command = lib.getExe' cfg.ceph.package "ceph";
        id = cfg.ceph.id;
        keyring = "${credDir}/ceph-keyring";
      };
    }
    // lib.optionalAttrs (cfg.s3.credentialsFile != null) {
      s3.credentials = "${credDir}/s3-admin";
    }
  );
in
{
  options.services.clusterd = {
    enable = lib.mkEnableOption "clusterd, the platform API";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.clusterd;
      defaultText = lib.literalExpression "clusterd.packages.\${system}.clusterd";
      description = "The clusterd package.";
    };

    settings = lib.mkOption {
      type = toml.type;
      default = { };
      description = ''
        clusterd's config.toml (see internal/config/config.go): site, host,
        listen, metrics_listen, server_name, tls, hosts (with an optional rgw
        URL each), vips, vms, s3.endpoint, s3.data_pool, limits. tokens_file,
        kubernetes, ceph and s3.credentials are set from the options below.
      '';
    };

    tokensFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        File with one "<name> <scope> sha256:<hex>" line per token (from
        `labctl token new`). It is handed over as a systemd credential, which
        systemd copies at start: restart the service after changing it (a
        reload re-reads the copy, and only the certificate changes on reload).
      '';
    };

    kubeconfigFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Kubeconfig with a ServiceAccount token and inline CA, for node status.";
    };

    ceph = {
      keyringFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = "Keyring of a read-only Ceph client (mon 'allow r'), for cluster health.";
      };
      id = lib.mkOption {
        type = lib.types.str;
        default = "clusterd";
        description = "Ceph client name, without the client. prefix.";
      };
      package = lib.mkPackageOption pkgs "ceph" { };
    };

    s3.credentialsFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        JSON {"access_key", "secret_key"} of an RGW user with the caps
        buckets=read;users=read;usage=read;metadata=read, for bucket usage in
        /v1/status/s3. Set settings.s3.endpoint and each host's rgw URL too.
      '';
    };

    manageUnitPrefixes = lib.mkOption {
      type = lib.types.listOf (lib.types.strMatching "[A-Za-z0-9@_.-]+");
      default = [ ];
      example = [ "microvm@" ];
      description = ''
        Units clusterd may restart, start and stop (through polkit), by name
        prefix: "microvm@" allows microvm@<anything>.service and nothing else.
        Empty: clusterd can't manage any unit.
      '';
    };

    extraGroups = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Extra groups for the service, e.g. to read a TLS certificate.";
    };
  };

  config = lib.mkIf cfg.enable {
    security.polkit = lib.mkIf (cfg.manageUnitPrefixes != [ ]) {
      enable = lib.mkDefault true;
      # Only these verbs, only these units, only this user.
      extraConfig = ''
        polkit.addRule(function(action, subject) {
          if (action.id != "org.freedesktop.systemd1.manage-units" || subject.user != "clusterd") {
            return polkit.Result.NOT_HANDLED;
          }
          var unit = action.lookup("unit") || "";
          var verb = action.lookup("verb") || "";
          var prefixes = ${builtins.toJSON cfg.manageUnitPrefixes};
          var allowed = ["restart", "start", "stop"];
          for (var i = 0; i < prefixes.length; i++) {
            if (unit.indexOf(prefixes[i]) === 0 && /^[A-Za-z0-9@_.:-]+\.service$/.test(unit) &&
                allowed.indexOf(verb) >= 0) {
              return polkit.Result.YES;
            }
          }
          return polkit.Result.NOT_HANDLED;
        });
      '';
    };

    # A fixed user: polkit rules (v1.0 actions) need a stable name.
    users.users.clusterd = {
      isSystemUser = true;
      group = "clusterd";
      description = "clusterd platform API";
    };
    users.groups.clusterd = { };

    environment.etc."clusterd/config.toml".source = toml.generate "clusterd.toml" settings;

    systemd.services.clusterd = {
      description = "clusterd platform API";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      restartTriggers = [ config.environment.etc."clusterd/config.toml".source ];
      # systemctl for VM state; the Ceph CLI is called by absolute path.
      path = [ config.systemd.package ];
      serviceConfig = {
        ExecStartPre = "${lib.getExe cfg.package} --config /etc/clusterd/config.toml --check";
        ExecStart = "${lib.getExe cfg.package} --config /etc/clusterd/config.toml";
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        RestartSec = 2;
        User = "clusterd";
        Group = "clusterd";
        SupplementaryGroups = cfg.extraGroups;
        LoadCredential = lib.mapAttrsToList (name: path: "${name}:${path}") credentials;

        # Port 443 without root.
        AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
        CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];
        NoNewPrivileges = true;

        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectHostname = true;
        ProtectClock = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectProc = "default";
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        RemoveIPC = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" "~@privileged" "~@resources" ];
        UMask = "0077";
        # Ceph's CLI is Python and keeps a small cache.
        CacheDirectory = "clusterd";
        Environment = [ "HOME=/var/cache/clusterd" ];
      };
    };
  };
}
