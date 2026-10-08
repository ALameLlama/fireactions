{
  lib,
  stdenv,
  kernelStdenv ? stdenv,
  linuxKernel,
  linuxManualConfig,
  writeText,
  sourceKernel ? linuxKernel.kernels.linux_6_12,
  configfile ? null,
  extraConfig ? "",
}:

let
  platform = kernelStdenv.hostPlatform;
  image =
    {
      x86_64-linux = "vmlinux";
      aarch64-linux = "arch/arm64/boot/Image";
    }
    .${platform.system}
      or (throw "Fireactions guest kernels support only x86_64-linux and aarch64-linux");
  inherit (sourceKernel) src version;
  nativeConfig =
    makeTarget:
    (linuxKernel.linuxConfig { inherit src version makeTarget; }).overrideAttrs (old: {
      buildPhase = lib.replaceStrings
        [ "ARCH=${stdenv.hostPlatform.linuxArch}" ]
        [ "ARCH=${platform.linuxArch}" ]
        old.buildPhase;
    });
  baseline = nativeConfig "defconfig";
  required = [
    "SMP"
    "PRINTK"
    "BLK_DEV_INITRD"
    "DEVTMPFS"
    "DEVTMPFS_MOUNT"
    "BINFMT_ELF"
    "BINFMT_SCRIPT"
    "EXT4_FS"
    "OVERLAY_FS"
    "TMPFS"
    "TMPFS_POSIX_ACL"
    "PROC_FS"
    "SYSFS"
    "CGROUPS"
    "CGROUP_PIDS"
    "CGROUP_FREEZER"
    "CGROUP_SCHED"
    "CGROUP_BPF"
    "MEMCG"
    "CPUSETS"
    "NAMESPACES"
    "UTS_NS"
    "IPC_NS"
    "USER_NS"
    "PID_NS"
    "NET_NS"
    "SECCOMP"
    "SECCOMP_FILTER"
    "BPF"
    "BPF_SYSCALL"
    "EVENTFD"
    "EPOLL"
    "SIGNALFD"
    "TIMERFD"
    "FHANDLE"
    "INOTIFY_USER"
    "NET"
    "INET"
    "IPV6"
    "IP_PNP"
    "IP_PNP_DHCP"
    "PACKET"
    "UNIX"
    "VETH"
    "BRIDGE"
    "BRIDGE_NETFILTER"
    "NETFILTER"
    "NETFILTER_ADVANCED"
    "NETFILTER_XTABLES"
    "NF_CONNTRACK"
    "NF_NAT"
    "NETFILTER_XT_MATCH_ADDRTYPE"
    "NETFILTER_XT_MATCH_CONNTRACK"
    "NETFILTER_XT_MARK"
    "NETFILTER_XT_TARGET_MASQUERADE"
    "IP_NF_IPTABLES"
    "IP_NF_FILTER"
    "IP_NF_NAT"
    "IP_NF_RAW"
    "IP6_NF_IPTABLES"
    "IP6_NF_FILTER"
    "IP6_NF_NAT"
    "IP6_NF_RAW"
    "VIRTIO"
    "VIRTIO_MMIO"
    "VIRTIO_BLK"
    "VIRTIO_NET"
    "VIRTIO_BALLOON"
    "VSOCKETS"
    "VIRTIO_VSOCKETS"
    "HW_RANDOM"
    "HW_RANDOM_VIRTIO"
    "SERIAL_8250"
    "SERIAL_8250_CONSOLE"
    "IKCONFIG"
    "IKCONFIG_PROC"
  ]
  ++ lib.optionals platform.isx86_64 [
    "ACPI"
    "PCI"
    "KVM_GUEST"
    "SERIO_I8042"
    "KEYBOARD_ATKBD"
  ]
  ++ lib.optionals platform.isAarch64 [
    "ARM64_4K_PAGES"
    "ARM_AMBA"
    "RTC_DRV_PL031"
    "SERIAL_OF_PLATFORM"
  ]
  # Linux 6.17 requires this separate option for legacy iptables.
  ++ lib.optional (lib.versionAtLeast version "6.17") "NETFILTER_XTABLES_LEGACY";
  generatedConfig =
    (nativeConfig "olddefconfig").overrideAttrs
      (old: {
        postPatch = old.postPatch + ''
          cp ${baseline} .config
          # All guest drivers must be built in because the image has no kernel modules.
          scripts/config --disable MODULES --disable RUST --disable DEBUG_INFO \
            --disable DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT \
            --disable VIRTIO_MMIO_CMDLINE_DEVICES \
            --set-str SYSTEM_TRUSTED_KEYS "" --set-str SYSTEM_REVOCATION_KEYS "" \
            ${lib.concatMapStringsSep " " (symbol: "--enable ${symbol}") required}
          cat ${writeText "fireactions-extra-kernel.config" extraConfig} >> .config
        '';
      });
in
(linuxManualConfig {
  inherit src version;
  stdenv = kernelStdenv;
  configfile = if configfile == null then generatedConfig else configfile;
  target = if platform.isAarch64 then "Image" else "vmlinux";
  buildDTBs = false;
  # The Firecracker kernel does not use modules, Rust, or an initramfs.
  config = { };
}).overrideAttrs
  (old: {
    configurePhase = lib.replaceStrings [ "oldconfig" ] [ "olddefconfig" ] old.configurePhase;
    # Check the resolved configuration before linuxManualConfig runs prepare or compiles the kernel.
    postConfigure = (old.postConfigure or "") + ''
      for symbol in ${lib.concatStringsSep " " required}; do
        if ! grep -qx "CONFIG_$symbol=y" "$buildRoot/.config"; then
          echo "Fireactions guest kernel requires CONFIG_$symbol=y" >&2
          exit 1
        fi
      done
      if grep -E '^(CONFIG_MODULES|CONFIG_RUST)=y$|^CONFIG_.*=m$' "$buildRoot/.config"; then
        echo "Fireactions guest kernel does not support loadable modules or Rust." >&2
        exit 1
      fi
    '';
    preInstall = "";
    installPhase = ''
      cp ${image} "$out"
    '';
    dontFixup = true;
    meta = {
      description = "Fireactions Linux ${version} guest kernel with Docker raw table support";
      homepage = "https://kernel.org";
      license = lib.licenses.gpl2Only;
      platforms = [
        "x86_64-linux"
        "aarch64-linux"
      ];
    };
  })
