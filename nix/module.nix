{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
let
  cfg = config.services.fireactions;
  inherit (lib)
    mkDefault
    mkEnableOption
    mkIf
    mkMerge
    mkOption
    types
    ;
  yaml = pkgs.formats.yaml { };
  configFile = yaml.generate "fireactions.yaml" cfg.settings;
  socketDirectory = builtins.dirOf cfg.settings.socket_path;
  runtimePath = types.strMatching "/.*";
  storageDevice =
    if cfg.devmapper.provision.enable && cfg.devmapper.provision.device != null then
      cfg.devmapper.provision.device
    else
      "/dev/mapper/${cfg.devmapper.poolName}";
  guestImageModule = { config, ... }: {
    options = {
      ubuntuVersion = mkOption {
        type = types.enum [
          "20.04"
          "22.04"
          "24.04"
        ];
        default = "24.04";
        description = "Pinned full Ubuntu image with native Docker and systemd. A custom fromImage overrides this base.";
      };
      fromImage = mkOption {
        type = types.nullOr types.package;
        default = null;
        description = "Custom native-architecture archive with systemd and the required guest tools. Null selects the pinned full Ubuntu image.";
      };
      name = mkOption {
        type = types.nonEmptyStr;
        default = "localhost/fireactions-guest";
        description = "Container image name for the generated guest archive.";
      };
      tag = mkOption {
        type = types.nonEmptyStr;
        default = "ubuntu-${config.ubuntuVersion}";
        description = "Container image tag for the generated guest archive.";
      };
      docker = {
        enable = mkOption {
          type = types.bool;
          default = true;
          description = "Start the Ubuntu image's Docker service before the guest agent. False disables Docker services but keeps the image's installed tools.";
        };
        settings = mkOption {
          type = types.attrsOf (pkgs.formats.json { }).type;
          default = { };
          description = "Guest Docker daemon configuration written to /etc/docker/daemon.json. These values enter the Nix store.";
        };
      };
      packages = mkOption {
        type = types.listOf types.package;
        default = [ ];
        example = lib.literalExpression "[ pkgs.php ]";
        description = "Additional guest packages. Their binaries are available in /usr/bin.";
      };
      extraCommands = mkOption {
        type = types.lines;
        default = "";
        description = "Shell commands appended after guest setup. The working directory is the image root, not a running guest.";
      };
      fakeRootCommands = mkOption {
        type = types.lines;
        default = "";
        description = "Shell commands appended after guest ownership and permissions are set under fakeroot.";
      };
    };
  };
  buildGuestImage =
    image:
    pkgs.callPackage ./guest-image.nix (
      {
        fireactions = cfg.package;
        inherit (image) ubuntuVersion name tag;
        dockerEnabled = image.docker.enable;
        dockerSettings = image.docker.settings;
        extraPackages = image.packages;
        extraCommands = image.extraCommands;
        fakeRootCommands = image.fakeRootCommands;
      }
      // lib.optionalAttrs (image.fromImage != null) { inherit (image) fromImage; }
    );
  defaultProfile = "ubuntu-${cfg.guestImage.ubuntuVersion}";
  hostPath = [
    pkgs.containerd
    config.services.lvm.package
    pkgs.iptables
    pkgs.iproute2
    pkgs.util-linux
  ];
  binary = lib.getExe cfg.package;
  imageArchives = lib.unique (
    lib.filter (archive: archive != null) (
      [ cfg.imageArchive ]
      ++ lib.mapAttrsToList (_: profile: profile.imageArchive) (
        lib.filterAttrs (_: profile: profile.enable) cfg.pools
      )
    )
  );
  hasArchive = imageArchives != [ ];
  serviceRestartTriggers = [
    configFile
    cfg.package
    cfg.guestKernel
  ]
  ++ imageArchives;
  pool =
    name: profile:
    lib.recursiveUpdate {
      inherit name;
      replicas = profile.prewarmCount;
      image = profile.image;
      image_pull_policy = profile.imagePullPolicy;
      default_user = profile.defaultUser;
      firecracker = {
        binary_path = profile.firecrackerBinary;
        kernel_image_path = toString profile.kernelImage;
        kernel_args = profile.kernelArgs;
        machine_config = {
          vcpu_count = profile.vcpuCount;
          mem_size_mib = profile.memoryMiB;
        };
      };
    } profile.settings;
  defaultPools = {
    ${defaultProfile} = {
      prewarmCount = 1;
      vcpuCount = 2;
      memoryMiB = 4096;
    };
    "${defaultProfile}-large" = {
      prewarmCount = 0;
      vcpuCount = 4;
      memoryMiB = 8192;
    };
  };
  defaultSettings = {
    containerd = {
      address = "/run/containerd/containerd.sock";
      namespace = "fireactions";
    };
    log_level = "info";
    socket_path = "/run/fireactions/plugin.sock";
    socket_group = "fireactions";
    state_dir = "/var/lib/fireactions";
    network.resolver_path = "/run/systemd/resolve/resolv.conf";
    guest = {
      startup_timeout = "2m";
      max_transfer_bytes = 10737418240;
      max_archive_entries = 100000;
    };
    leases = {
      max_lifetime = "3h2m";
      cleanup_grace = "2m";
      reap_interval = "10s";
    };
    pools = lib.mapAttrsToList pool (lib.filterAttrs (_: profile: profile.enable) cfg.pools);
  };
  runnerConfig = yaml.generate "fireactions-runner.yaml" {
    runner = {
      capacity = cfg.runner.capacity;
      timeout = "30m";
      shutdown_timeout = "35m";
      labels = cfg.runner.labels;
    };
    cache = {
      enabled = cfg.runner.cache.enable;
      proxy_port = 8088;
    };
    container.docker_host = "-";
    plugins.firecracker.address = "unix://${cfg.settings.socket_path}";
    server.connections.forgejo = {
      inherit (cfg.runner) url uuid;
      token_url = "file:///run/credentials/fireactions-runner.service/token";
    };
  };
  cniConfig = {
    cniVersion = "1.0.0";
    name = "fireactions";
    plugins = [
      {
        type = "bridge";
        bridge = cfg.networking.bridge;
        isDefaultGateway = true;
        ipMasq = true;
        hairpinMode = true;
        ipam = {
          type = "host-local";
          subnet = cfg.networking.subnet;
          resolvConf = cfg.settings.network.resolver_path;
        };
      }
      { type = "firewall"; }
      { type = "tc-redirect-tap"; }
    ];
  };
in
{
  options.services.fireactions = {
    enable = mkEnableOption "the Fireactions Firecracker execution backend";
    package = mkOption {
      type = types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "Fireactions host binary built from this fork.";
    };
    guestKernelPackage = mkOption {
      type = types.package;
      default = pkgs.linuxKernel.kernels.linux_6_12;
      defaultText = lib.literalExpression "pkgs.linuxKernel.kernels.linux_6_12";
      example = lib.literalExpression "pkgs.linuxKernel.kernels.linux_6_18";
      description = "Upstream Linux package that supplies the guest kernel source and version. Fireactions builds its own configuration without Amazon patches.";
    };
    guestKernelConfig = mkOption {
      type = types.lines;
      default = "";
      example = "CONFIG_NF_TABLES=y";
      description = "Kconfig lines appended to the guest kernel baseline before olddefconfig. Required Docker support remains enabled.";
    };
    guestKernel = mkOption {
      type = types.package;
      default = pkgs.callPackage ./guest-kernel.nix {
        sourceKernel = cfg.guestKernelPackage;
        extraConfig = cfg.guestKernelConfig;
      };
      defaultText = lib.literalExpression "pkgs.callPackage ./guest-kernel.nix { sourceKernel = config.services.fireactions.guestKernelPackage; extraConfig = config.services.fireactions.guestKernelConfig; }";
      description = "Architecture-matched guest kernel file derivation. Override the package to select custom source, configuration, or compiler.";
    };
    tcRedirectTapPackage = mkOption {
      type = types.package;
      default = pkgs.callPackage ./tc-redirect-tap.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./tc-redirect-tap.nix { }";
      description = "tc-redirect-tap CNI plugin package.";
    };
    pools = mkOption {
      type = types.attrsOf (
        types.submodule (
          { config, name, ... }: {
            options = {
              enable = mkOption {
                type = types.bool;
                default = true;
                description = "Whether to make this guest profile available.";
              };
              prewarmCount = mkOption {
                type = types.ints.between 0 2147483647;
                default = 0;
                description = "Number of clean idle VMs to keep ready for this profile.";
              };
              vcpuCount = mkOption {
                type = types.ints.positive;
                default = 2;
                description = "Number of virtual CPUs in each guest.";
              };
              memoryMiB = mkOption {
                type = types.ints.positive;
                default = 4096;
                description = "Addressable guest RAM in MiB. The host allocates memory on demand.";
              };
              image = mkOption {
                type = types.nonEmptyStr;
                default =
                  let
                    image = if config.guestImage == null then cfg.guestImage else config.guestImage;
                  in
                  "${image.name}:${image.tag}";
                description = "Bootable guest image reference in containerd.";
              };
              imageArchive = mkOption {
                type = types.nullOr (types.either types.package runtimePath);
                default = if config.guestImage == null then null else buildGuestImage config.guestImage;
                description = ''
                  Additional guest archive to import for this enabled pool.
                  Use a derivation built with the host package or an absolute runtime filename.
                  Archive changes use the same graceful import lifecycle as the default image.
                  Null does not add an archive; the pool image must already be imported.
                '';
              };
              guestImage = mkOption {
                type = types.nullOr (
                  types.submodule [
                    guestImageModule
                    { name = mkDefault "${cfg.guestImage.name}-${name}"; }
                  ]
                );
                default = null;
                description = "Build a matching guest archive for this pool. Null shares the default image or an explicitly selected archive.";
              };
              imagePullPolicy = mkOption {
                type = types.enum [
                  "Always"
                  "Never"
                  "IfNotPresent"
                ];
                default = "Never";
                description = "When to pull the guest image.";
              };
              defaultUser = mkOption {
                type = types.strMatching "[a-z_][a-z0-9_-]{0,31}";
                default = "ci";
                description = "Guest user that runs workflow commands.";
              };
              firecrackerBinary = mkOption {
                type = runtimePath;
                default = lib.getExe pkgs.firecracker;
                defaultText = lib.literalExpression "lib.getExe pkgs.firecracker";
                description = "Absolute filename of the Firecracker executable.";
              };
              kernelImage = mkOption {
                type = types.either types.package runtimePath;
                default = cfg.guestKernel;
                defaultText = lib.literalExpression "config.services.fireactions.guestKernel";
                description = "Guest kernel file derivation or absolute runtime filename.";
              };
              kernelArgs = mkOption {
                type = types.str;
                default = "console=ttyS0 reboot=k panic=1 pci=off nomodules rw init=/sbin/init systemd.unified_cgroup_hierarchy=1";
                description = "Guest kernel command line.";
              };
              settings = mkOption {
                type = types.attrsOf yaml.type;
                default = { };
                description = "Additional backend YAML fields for this pool. These values override generated fields and enter the Nix store.";
              };
            };
          }
        )
      );
      default = { };
      description = "Named guest profiles. Standard and large Ubuntu defaults merge with per-pool overrides. Explicit settings.pools replaces the generated pool list.";
    };
    settings = mkOption {
      type = yaml.type;
      default = { };
      description = ''
        Fireactions YAML settings. Attribute overrides merge with host defaults;
        setting pools replaces the entire default pool list. This configuration
        enters the Nix store: do not put tokens or other secrets here.
      '';
    };
    devmapper = {
      poolName = mkOption {
        type = types.str;
        default =
          if cfg.devmapper.provision.enable then
            "${lib.replaceStrings [ "-" ] [ "--" ] cfg.devmapper.provision.volumeGroup}-${lib.replaceStrings [ "-" ] [ "--" ] cfg.devmapper.provision.logicalVolume}"
          else
            "";
        example = "containerd-thinpool";
        description = "Device-mapper name of the thin pool. Required for existing storage and derived from the managed volume names when provisioning is enabled.";
      };
      baseImageSize = mkOption {
        type = types.str;
        default = "30GB";
        description = "Containerd devmapper base image size.";
      };
      provision = {
        enable = mkEnableOption "one-time LVM thin-pool creation on an explicitly selected empty spare device";
        device = mkOption {
          type = types.nullOr runtimePath;
          default = null;
          example = "/dev/disk/by-id/REPLACE_WITH_VERIFIED_EMPTY_SPARE_DEVICE";
          description = "Empty dedicated block device. Provisioning writes LVM metadata and refuses existing signatures, partitions, holders, or unrelated LVM storage.";
        };
        volumeGroup = mkOption {
          type = types.strMatching "[a-zA-Z0-9_+][a-zA-Z0-9_+.-]*";
          default = "containerd";
          description = "Name of the volume group created on the dedicated device.";
        };
        logicalVolume = mkOption {
          type = types.strMatching "[a-zA-Z0-9_+][a-zA-Z0-9_+.-]*";
          default = "thinpool";
          description = "Name of the managed thin pool.";
        };
        initialSize = mkOption {
          type = types.ints.between 1 95;
          default = 80;
          description = "Percentage of free volume-group extents used at creation. Rebuilds do not resize the pool.";
        };
        metadataSize = mkOption {
          type = types.strMatching "[1-9][0-9]*[mMgGtT]";
          default = "1G";
          description = "Initial thin-pool metadata size. Rebuilds do not change this size.";
        };
      };
    };
    networking = {
      subnet = mkOption {
        type = types.strMatching "[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+/[0-9]+";
        default = "192.168.128.0/24";
        description = "IPv4 guest subnet in CIDR notation; choose one that does not overlap host, VPN, or other container networks.";
      };
      bridge = mkOption {
        type = types.strMatching "[a-zA-Z0-9_.-]{1,15}";
        default = "fireactions-br0";
        description = "Dedicated CNI bridge interface name.";
      };
    };
    guestImage = mkOption {
      type = types.submodule guestImageModule;
      default = { };
      description = "Default guest image recipe. Enabled pools can define independent recipes under pools.<name>.guestImage.";
    };
    imageArchive = mkOption {
      type = types.nullOr (types.either types.package runtimePath);
      default = buildGuestImage cfg.guestImage;
      defaultText = lib.literalMD "The matching archive configured by `services.fireactions.guestImage`.";
      example = "/var/lib/fireactions-images/ubuntu-24.04.tar";
      description = ''
        A native-architecture OCI/Docker image archive derivation or an absolute runtime filename.
        The default archive contains the same Fireactions package as the host.
        Import the archive with devmapper before the backend starts and after containerd restarts.
        Changes to the archive derivation drain the runner and restart the import and backend.
        Runtime strings remain outside the Nix store and have no file watcher.
        After replacing a runtime archive, restart fireactions-image-import.service to drain the runner and import it.
        When null, only archives from enabled pools are managed.
        Import any other configured images separately.
      '';
    };
    runner = {
      enable = mkEnableOption "an unprivileged Forgejo Runner using the Fireactions plugin";
      cache.enable = mkEnableOption "the Forgejo Runner cache proxy for guest jobs";
      package = mkOption {
        type = types.package;
        default = pkgs.forgejo-runner;
        defaultText = lib.literalExpression "pkgs.forgejo-runner";
        description = "Forgejo Runner 13.2 or newer, with execution plugin support.";
      };
      url = mkOption {
        type = types.str;
        default = "";
        example = "https://forgejo.example.org/";
        description = "URL of the Forgejo instance where the runner is already registered.";
      };
      uuid = mkOption {
        type = types.str;
        default = "";
        description = "UUID from an existing Forgejo runner registration; this module does not register runners.";
      };
      tokenFile = mkOption {
        type = types.nullOr runtimePath;
        default = null;
        example = "/run/secrets/fireactions-runner-token";
        description = ''
          Absolute runtime filename containing the registered runner token.
          Use a string, not a Nix path. The root service manager loads it with
          LoadCredential; the token must never be placed in the Nix store.
        '';
      };
      capacity = mkOption {
        type = types.ints.positive;
        default = 1;
        description = "Maximum concurrent runner jobs.";
      };
      labels = mkOption {
        type = types.listOf types.str;
        default = [ "firecracker:firecracker://${defaultProfile}" ];
        description = "Runner labels selecting configured Fireactions pool profiles.";
      };
    };
  };

  config = mkIf cfg.enable (mkMerge [
    {
      assertions = [
        {
          assertion = cfg.devmapper.poolName != "";
          message = "services.fireactions.devmapper.poolName must name an existing activated thin pool, or enable devmapper.provision with an explicitly selected empty spare device.";
        }
        {
          assertion = !cfg.devmapper.provision.enable || cfg.devmapper.provision.device != null;
          message = "services.fireactions.devmapper.provision.device must identify a known empty spare device when provisioning is enabled.";
        }
        {
          assertion =
            !cfg.devmapper.provision.enable
            || cfg.devmapper.poolName == "${lib.replaceStrings [ "-" ] [ "--" ] cfg.devmapper.provision.volumeGroup}-${lib.replaceStrings [ "-" ] [ "--" ] cfg.devmapper.provision.logicalVolume}";
          message = "services.fireactions.devmapper.poolName must match the provisioned LVM volume names.";
        }
        {
          assertion = !(builtins.elem "no_devmapper" (pkgs.containerd.tags or [ ]));
          message = "Fireactions requires containerd compiled with the devmapper snapshotter (remove the no_devmapper build tag).";
        }
      ];
      services.fireactions.settings = lib.mapAttrsRecursive (_: value: mkDefault value) defaultSettings;
      services.fireactions.pools = lib.mapAttrsRecursive (_: value: mkDefault value) defaultPools;
      services.resolved.enable = mkDefault true;
      services.lvm.enable = true;
      services.lvm.boot.thin.enable = true;
      environment.systemPackages = [
        cfg.package
        pkgs.firecracker
        pkgs.containerd
      ];
      users.groups.${cfg.settings.socket_group} = { };
      # ARM64 KVM is built into the NixOS kernel; x86 loads the common module
      # here and the CPU-specific kvm_intel/kvm_amd driver through modaliases.
      boot.kernelModules = [
        "dm_thin_pool"
        "tun"
      ]
      ++ lib.optional pkgs.stdenv.hostPlatform.isx86_64 "kvm";
      boot.kernel.sysctl = {
        "net.ipv4.ip_forward" = 1;
        "net.ipv4.conf.all.forwarding" = 1;
        "net.ipv4.conf.all.rp_filter" = mkDefault 2;
        "net.ipv4.conf.default.rp_filter" = mkDefault 2;
      };
      networking.firewall.checkReversePath = mkDefault "loose";
      environment.etc."cni/net.d/10-fireactions.conflist".text = builtins.toJSON cniConfig;
      environment.etc."fireactions/config.yaml".source = configFile;
      # Fireactions validates and creates its own state/socket directories;
      # tmpfiles must not chmod arbitrary directories from settings overrides.
      systemd.tmpfiles.rules = [
        "d /opt/cni/bin 0755 root root - -"
        "L+ /opt/cni/bin/tc-redirect-tap - - - - ${cfg.tcRedirectTapPackage}/bin/tc-redirect-tap"
      ]
      ++ map (plugin: "L+ /opt/cni/bin/${plugin} - - - - ${pkgs.cni-plugins}/bin/${plugin}") [
        "bridge"
        "firewall"
        "host-local"
        "loopback"
      ];

      # The pinned containerd includes devmapper by default: its Linux builtin
      # is guarded by !no_devmapper, and nixpkgs does not set that build tag.
      virtualisation.containerd = {
        enable = true;
        settings.plugins."io.containerd.snapshotter.v1.devmapper" = {
          pool_name = cfg.devmapper.poolName;
          root_path = "/var/lib/containerd/devmapper";
          base_image_size = cfg.devmapper.baseImageSize;
          discard_blocks = true;
        };
      };
      systemd.services.containerd = {
        requires = [ "fireactions-storage.service" ];
        after = [ "fireactions-storage.service" ];
        path = [
          config.services.lvm.package
          pkgs.util-linux
          pkgs.e2fsprogs
          pkgs.coreutils
        ];
        wants = lib.optional hasArchive "fireactions-image-import.service";
      };
      systemd.services.fireactions-storage = {
        description = "Prepare and validate Fireactions thin-pool storage";
        requires = [
          "${utils.escapeSystemdPath storageDevice}.device"
        ];
        after = [
          "${utils.escapeSystemdPath storageDevice}.device"
        ] ++ lib.optional cfg.devmapper.provision.enable "lvm2-monitor.service";
        before = [ "containerd.service" ];
        path = [
          config.services.lvm.package
          pkgs.util-linux
          pkgs.coreutils
          pkgs.gnugrep
        ];
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          UMask = "0077";
        };
        script =
          if cfg.devmapper.provision.enable then
            ''
              device=${lib.escapeShellArg storageDevice}
              volumeGroup=${lib.escapeShellArg cfg.devmapper.provision.volumeGroup}
              logicalVolume=${lib.escapeShellArg cfg.devmapper.provision.logicalVolume}
              initialSize=${toString cfg.devmapper.provision.initialSize}
              metadataSize=${lib.escapeShellArg cfg.devmapper.provision.metadataSize}
              poolName=${lib.escapeShellArg cfg.devmapper.poolName}
              source ${./storage-setup.sh}
            ''
          else
            ''
              dmsetup table ${lib.escapeShellArg cfg.devmapper.poolName} | grep -qw thin-pool
            '';
      };
      systemd.services.fireactions = {
        description = "Fireactions Firecracker execution backend";
        wantedBy = [ "multi-user.target" ];
        wants = [ "network-online.target" ];
        requires = [
          "containerd.service"
          "network-online.target"
        ]
        ++ lib.optional hasArchive "fireactions-image-import.service";
        after = [
          "containerd.service"
          "network-online.target"
          "systemd-tmpfiles-setup.service"
        ]
        ++ lib.optional config.services.resolved.enable "systemd-resolved.service"
        ++ lib.optional hasArchive "fireactions-image-import.service";
        partOf = [ "containerd.service" ] ++ lib.optional hasArchive "fireactions-image-import.service";
        path = hostPath;
        restartTriggers = serviceRestartTriggers;
        preStart = "${binary} validate --host ${configFile}";
        serviceConfig = {
          User = "root";
          Group = "root";
          UMask = "0077";
          ExecStart = "${binary} server --config ${configFile}";
          Restart = "on-failure";
          RestartSec = "5s";
          KillMode = "control-group";
          TimeoutStopSec = "30s";
        };
      };
      systemd.services.fireactions-reaper = {
        description = "Reap expired or abandoned Fireactions environments";
        requires = [ "containerd.service" ];
        after = [
          "containerd.service"
          "systemd-tmpfiles-setup.service"
        ];
        path = hostPath;
        restartTriggers = [ configFile ];
        serviceConfig = {
          Type = "oneshot";
          User = "root";
          Group = "root";
          UMask = "0077";
          ExecStart = "${binary} reap --config ${configFile}";
        };
      };
      systemd.timers.fireactions-reaper = {
        description = "Periodically reap Fireactions environments independently of the daemon";
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnBootSec = "10s";
          OnUnitActiveSec = "10s";
          AccuracySec = "1s";
          Unit = "fireactions-reaper.service";
        };
      };
    }
    (mkIf cfg.devmapper.provision.enable {
      services.lvm.dmeventd.enable = true;
      environment.etc."lvm/profile.d/fireactions.profile".text = ''
        activation {
          thin_pool_autoextend_threshold = 80
          thin_pool_autoextend_percent = 20
        }
      '';
    })
    (mkIf (!config.networking.nftables.enable) {
      networking.firewall.extraCommands = ''
        iptables -w -I FORWARD -i ${cfg.networking.bridge} -s ${cfg.networking.subnet} -j ACCEPT
        iptables -w -I FORWARD -o ${cfg.networking.bridge} -d ${cfg.networking.subnet} -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
      '';
      networking.firewall.extraStopCommands = ''
        iptables -w -D FORWARD -i ${cfg.networking.bridge} -s ${cfg.networking.subnet} -j ACCEPT 2>/dev/null || true
        iptables -w -D FORWARD -o ${cfg.networking.bridge} -d ${cfg.networking.subnet} -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true
      '';
    })
    (mkIf config.networking.nftables.enable {
      networking.firewall.extraForwardRules = ''
        iifname "${cfg.networking.bridge}" ip saddr ${cfg.networking.subnet} accept
        oifname "${cfg.networking.bridge}" ip daddr ${cfg.networking.subnet} ct state established,related accept
      '';
    })
    (mkIf hasArchive {
      systemd.services.fireactions-image-import = {
        description = "Import the Fireactions guest archives into containerd devmapper";
        requires = [ "containerd.service" ];
        after = [ "containerd.service" ];
        before = [ "fireactions.service" ];
        partOf = [ "containerd.service" ];
        path = hostPath;
        restartTriggers = serviceRestartTriggers;
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          User = "root";
          UMask = "0077";
        };
        script = lib.concatMapStringsSep "\n" (archive: ''
          ${pkgs.containerd}/bin/ctr --address ${lib.escapeShellArg cfg.settings.containerd.address} \
            --namespace ${lib.escapeShellArg cfg.settings.containerd.namespace} \
            images import --local --snapshotter devmapper ${lib.escapeShellArg (toString archive)}
        '') imageArchives;
      };
    })
    (mkIf cfg.runner.enable {
      assertions = [
        {
          assertion = lib.versionAtLeast (cfg.runner.package.version or "0") "13.2";
          message = "services.fireactions.runner requires Forgejo Runner >= 13.2 with execution plugin support; the selected package reports ${
            cfg.runner.package.version or "no version"
          }.";
        }
        {
          assertion = cfg.runner.url != "" && cfg.runner.uuid != "";
          message = "services.fireactions.runner.url and uuid must identify an already registered Forgejo runner; automatic registration is disabled.";
        }
        {
          assertion =
            cfg.runner.tokenFile != null && !(lib.hasPrefix "${builtins.storeDir}/" cfg.runner.tokenFile);
          message = "services.fireactions.runner.tokenFile must be an absolute runtime string outside the Nix store, containing the registered runner token.";
        }
        {
          assertion =
            cfg.settings.state_dir != "/var/lib/fireactions-runner"
            && !(lib.hasPrefix "${cfg.settings.state_dir}/" "/var/lib/fireactions-runner")
            && !(lib.hasPrefix "/var/lib/fireactions-runner/" cfg.settings.state_dir)
            && !(lib.hasPrefix "${cfg.settings.state_dir}/" socketDirectory)
            && socketDirectory != cfg.settings.state_dir;
          message = "Fireactions root-private state must be separate from the runner's /var/lib/fireactions-runner state and its accessible plugin socket directory.";
        }
      ];
      networking.firewall.interfaces = mkIf cfg.runner.cache.enable {
        ${cfg.networking.bridge}.allowedTCPPorts = [ 8088 ];
      };
      users.groups.fireactions-runner = { };
      users.users.fireactions-runner = {
        isSystemUser = true;
        group = "fireactions-runner";
        extraGroups = [ cfg.settings.socket_group ];
        home = "/var/lib/fireactions-runner";
      };
      systemd.services.fireactions-runner = {
        description = "Forgejo Runner using the Fireactions execution plugin";
        wantedBy = [ "multi-user.target" ];
        wants = [ "network-online.target" ];
        requires = [ "fireactions.service" ];
        after = [
          "network-online.target"
          "fireactions.service"
        ];
        partOf = [ "fireactions.service" ];
        restartTriggers = serviceRestartTriggers ++ [ runnerConfig ];
        serviceConfig = {
          User = "fireactions-runner";
          Group = "fireactions-runner";
          SupplementaryGroups = [ cfg.settings.socket_group ];
          StateDirectory = "fireactions-runner";
          StateDirectoryMode = "0700";
          WorkingDirectory = "/var/lib/fireactions-runner";
          UMask = "0077";
          LoadCredential = lib.optional (cfg.runner.tokenFile != null) "token:${cfg.runner.tokenFile}";
          ExecStart = "${lib.getExe cfg.runner.package} daemon --config ${runnerConfig}";
          Restart = "on-failure";
          RestartSec = "5s";
          TimeoutStopSec = "36min";
        };
      };
    })
  ]);
}
