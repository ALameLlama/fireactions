# Guest kernels

Firecracker boots each guest with a Linux kernel image. A configured profile names the kernel file in `firecracker.kernel_image_path`. The host must be able to read that file.

The example profiles use `/var/lib/fireactions/kernels/6.1/vmlinux`. Use an uncompressed `vmlinux` image that Firecracker can boot. Do not use a compressed `bzImage` file as the Firecracker kernel image.

## Kernel requirements

The guest kernel must support the devices and services used by the guest image. These include the serial console, virtio block and network devices, virtio vsock, and the platform-specific devices required by the guest architecture. The guest image also uses systemd and unified cgroup v2.

Use Linux 5.14 or newer, or a kernel with a backport that exposes `cgroup.kill` in unified cgroup v2. The guest agent requires writable `cgroup.procs` and `cgroup.kill` files to become ready and stop job processes. Cgroup v2 support alone is not sufficient.

The example kernel arguments start with `console=ttyS0` and do not include `noapic`. Keep SMP support enabled so a profile with multiple virtual CPUs can use its configured CPUs. Do not add `noapic` to the kernel command line. The example also uses `systemd.unified_cgroup_hierarchy=1` for cgroup v2.

Check that your Firecracker version supports the kernel and guest architecture that you select. See the [Firecracker kernel support policy](https://github.com/firecracker-microvm/firecracker/blob/main/docs/kernel-policy.md).

The optional installer setup uses a pinned Linux `6.1.128` image from the [official Firecracker CI bucket](https://s3.amazonaws.com/spec.ccfc.min/). It installs the image at `/var/lib/fireactions/kernels/6.1/vmlinux`. The prepared-host installer does not replace an existing kernel.

The Linux/amd64 deployment checks booted this image with the configured four-CPU profile and cgroup v2 guest agent. Make sure that your kernel and Firecracker version work together before you use a different host architecture.

## Build a custom kernel

Build a Linux kernel that meets the requirements above and produces an uncompressed `vmlinux` file. Use kernel source and a configuration that match your target architecture and Firecracker support policy. Install the resulting file on the host, then set `firecracker.kernel_image_path` for each profile that uses it.

For a profile with multiple CPUs, include SMP support in the kernel. For the supplied guest image, include systemd and unified cgroup v2 support with `cgroup.kill`. Keep the serial console and required virtio drivers available to the guest.

## Troubleshooting

If a guest does not boot, make sure that the configured kernel path points to a readable, uncompressed `vmlinux` file. Check the host Fireactions journal for Firecracker startup errors. Check that the kernel supports the configured architecture, virtio devices, SMP, and guest init system.
