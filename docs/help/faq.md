# Frequently asked questions

## What does Fireactions run?

Fireactions runs Forgejo workflows in disposable Firecracker virtual machines. A configured profile names a prepared, bootable Linux guest image and its VM resources. Fireactions does not run arbitrary Docker images as guest profiles.

## Which Runner and guest versions are supported?

The Forgejo runner plugin protocol is pinned to Runner 13.2 alpha. Linux guests are supported. The plugin does not support service containers or Docker-container actions. It also does not support stdin, PTY, or signal RPC. Capability adjustments are advisory and ignored.

## Can I connect to a guest with SSH or tmate?

No. Fireactions does not provide guest SSH login, a default password, or tmate access. Use the workflow output and `fireactions logs VM_ID` to inspect guest agent logs.

## Does Fireactions reuse virtual machines?

No. A VM that belongs to a job is disposable and never returns to the idle pool. Fireactions does not resume a job after a restart. The independent reaper cleans expired or abandoned resources.

## Does Fireactions add Firecracker jailer isolation?

No. Fireactions does not add jailer isolation. Run the host service with the required KVM, containerd, CNI, network, and root-mount privileges. Treat the guest image and workflow code as untrusted input, and protect the host and its credentials.

## Can I run Fireactions inside a virtual machine?

The host must provide usable KVM access and the required containerd, devmapper, CNI, kernel, and resolver resources. A nested virtual machine can have performance limits. Run `fireactions validate --host CONFIG` to check the configured host prerequisites.

## Does Fireactions support GPU workloads?

No. The current Firecracker VM setup does not provide GPU support.
