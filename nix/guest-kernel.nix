{ lib, stdenv, fetchurl }:

let
  kernels = {
    x86_64-linux = {
      arch = "x86_64";
      hash = "sha256-J6gxC5pydRfp6wIERSS2zrd95XKONJG2l01chGIn7Mg=";
    };
    aarch64-linux = {
      arch = "aarch64";
      hash = "sha256-yxKRxmvKdbwRy5yDV/zvmWW7F4bf/LQqYJI8Pg5J8xk=";
    };
  };
  kernel = kernels.${stdenv.hostPlatform.system}
    or (throw "Fireactions guest kernels support only x86_64-linux and aarch64-linux");
in
fetchurl {
  name = "fireactions-vmlinux-6.1.128-${kernel.arch}";
  url = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/${kernel.arch}/vmlinux-6.1.128";
  inherit (kernel) hash;

  meta = {
    description = "Firecracker CI Linux 6.1.128 guest kernel";
    homepage = "https://github.com/firecracker-microvm/firecracker";
    license = lib.licenses.gpl2Only;
    platforms = [ "x86_64-linux" "aarch64-linux" ];
  };
}
