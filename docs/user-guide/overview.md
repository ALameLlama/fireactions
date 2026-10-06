## What Fireactions does

Fireactions runs Forgejo Actions jobs in disposable Firecracker virtual machines (VMs). A VM is a virtual computer. Firecracker uses Linux KVM to run the VM on a Linux host.

Fireactions provides a Forgejo-compatible plugin through a local Unix socket. Forgejo Runner connects to this socket from the host. Runner protocol support is pinned to version 13.2, and the supported guest operating system is Linux.

Each configured profile names a bootable guest image, a kernel, CPU count, and memory size. A pool can keep clean, never-claimed VMs ready. After a job claims a VM, Fireactions destroys it at the end of its hard lease. It never returns a used VM to the idle pool.

## Host requirements

Use a Linux host with working KVM access, containerd with the devmapper snapshotter, the configured CNI network, Firecracker, and required CNI plugin executables. CNI provides network connectivity for guests. The host must have enough CPU, memory, and storage for the configured profiles.

The default install checks those existing resources. It does not overwrite containerd, LVM, or CNI configuration. The host service runs with privileges needed for KVM, containerd, CNI, networking, and guest root filesystems. This is a privileged host service, not a jailer-based isolation boundary.

The host resolver path must exist and contain usable nameservers. The guest networking setup must also reach the Forgejo server and any registries or services that jobs need. Host DNS settings alone do not prove that guests can reach those services.

See the [installation guide](installation.md) for setup, then read [core concepts](concepts.md) and [images](images.md).
