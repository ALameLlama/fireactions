{ pkgs, module, imageTools }:
let
  root = pkgs.buildEnv {
    name = "fireactions-import-test-root";
    paths = [
      pkgs.pkgsStatic.busybox
      (pkgs.writeTextDir "etc/fireactions-proof" "imported-rootfs\n")
    ];
    pathsToLink = [ "/bin" "/etc" ];
  };
  managedImage = imageTools.nix2container.buildImage {
    name = "localhost/fireactions-import-test";
    tag = "managed";
    copyToRoot = root;
  };
  externalArchive = pkgs.dockerTools.buildLayeredImage {
    name = "localhost/fireactions-import-test";
    tag = "archive";
    contents = [ root ];
    compressor = "none";
  };
in
pkgs.testers.runNixOSTest {
  name = "fireactions-storage";
  nodes.machine = { lib, ... }: {
    imports = [ module ];
    virtualisation.emptyDiskImages = [ 8192 8192 8192 ];
    services.fireactions = {
      enable = true;
      imageArchive = managedImage;
      pools.external = {
        image = "localhost/fireactions-import-test:archive";
        imageArchive = externalArchive;
        prewarmCount = 0;
      };
      guestKernel = pkgs.emptyFile;
      devmapper.provision = {
        enable = true;
        device = "/dev/vdb";
        metadataSize = "32M";
      };
    };
    services.dockerRegistry.package = pkgs.writeShellScriptBin "registry" ''
      sleep 2
      exec ${pkgs.distribution}/bin/registry "$@"
    '';
    # This test exercises storage, not guest boot or runner registration.
    systemd.services.fireactions.enable = lib.mkForce false;
    systemd.services.fireactions-reaper.enable = lib.mkForce false;
    systemd.timers.fireactions-reaper.enable = lib.mkForce false;
    environment.systemPackages = [ pkgs.e2fsprogs ];
  };
  testScript = ''
    import json

    machine.start(allow_reboot=True)
    machine.wait_for_unit("containerd.service")
    machine.wait_for_unit("fireactions-image-import.service")
    repositories = json.loads(machine.succeed("${pkgs.curl}/bin/curl --fail --silent http://127.0.0.1:5000/v2/_catalog"))["repositories"]
    layers = json.loads(machine.succeed("cat ${managedImage}"))["layers"]
    for layer in layers:
        machine.succeed(f"${pkgs.curl}/bin/curl --fail --silent --head http://127.0.0.1:5000/v2/{repositories[0]}/blobs/{layer['digest']}")
    for tag in ["managed", "archive"]:
        command = f"ctr --namespace fireactions run --rm --snapshotter devmapper localhost/fireactions-import-test:{tag} smoke-{tag} /bin/busybox cat /etc/fireactions-proof"
        assert machine.succeed(command).strip() == "imported-rootfs"
    machine.succeed("systemctl stop docker-registry.service")
    machine.succeed("systemctl start fireactions-image-import.service")
    assert machine.succeed("ctr --namespace fireactions run --rm --snapshotter devmapper localhost/fireactions-import-test:managed smoke-repeat /bin/busybox cat /etc/fireactions-proof").strip() == "imported-rootfs"
    machine.succeed("dmsetup table containerd-thinpool | grep -w thin-pool")
    assert machine.succeed("lvs --noheadings -o seg_monitor containerd/thinpool").strip() == "monitored"
    machine.succeed("ctr --namespace fireactions snapshots --snapshotter devmapper prepare retained")
    machine.succeed("mkdir -p /mnt/retained")
    mount_snapshot = machine.succeed("ctr --namespace fireactions snapshots --snapshotter devmapper mounts /mnt/retained retained").strip()
    machine.succeed(mount_snapshot)
    machine.succeed("printf retained-data > /mnt/retained/proof; sync")
    before = machine.succeed("pvs --noheadings -o pv_uuid /dev/vdb; lvs --noheadings -o lv_uuid,lv_size containerd/thinpool")
    machine.succeed("systemctl restart fireactions-storage.service")
    assert before == machine.succeed("pvs --noheadings -o pv_uuid /dev/vdb; lvs --noheadings -o lv_uuid,lv_size containerd/thinpool")
    machine.succeed("grep -x retained-data /mnt/retained/proof")

    setup = "device=/dev/vdc volumeGroup=other logicalVolume=thinpool initialSize=80 metadataSize=32M poolName=other-thinpool bash ${./storage-setup.sh}"
    machine.succeed("mkfs.ext4 /dev/vdc")
    filesystem = machine.succeed("blkid -s UUID -o value /dev/vdc")
    assert "existing signatures" in machine.fail(setup + " 2>&1")
    assert filesystem == machine.succeed("blkid -s UUID -o value /dev/vdc")

    machine.succeed("pvcreate /dev/vdd; vgcreate unrelated /dev/vdd; lvcreate -L 16M -n data unrelated")
    unrelated = machine.succeed("pvs --noheadings -o pv_uuid /dev/vdd; lvs --noheadings -o lv_uuid unrelated/data")
    assert "not managed" in machine.fail(setup.replace("/dev/vdc", "/dev/vdd") + " 2>&1")
    assert unrelated == machine.succeed("pvs --noheadings -o pv_uuid /dev/vdd; lvs --noheadings -o lv_uuid unrelated/data")

    machine.reboot()
    machine.wait_for_unit("containerd.service")
    machine.succeed("mkdir -p /mnt/retained")
    machine.succeed(mount_snapshot)
    machine.succeed("grep -x retained-data /mnt/retained/proof")
  '';
}
