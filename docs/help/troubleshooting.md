# Troubleshooting

## Host validation fails

Run schema validation first, then host validation with the configuration used by the service:

```sh
fireactions validate /etc/fireactions/config.yaml
sudo fireactions validate --host /etc/fireactions/config.yaml
```

The host check reports missing or unusable configured Firecracker and kernel paths, KVM access, containerd devmapper support, CNI plugins, or resolver settings. Install or configure the missing host prerequisite with your normal host-management procedure. The check does not change host resources.

## A profile cannot find its guest image

Check that the image name in the profile matches the imported image name. Check that `containerd.namespace` names the same namespace used to import the image. Check that the image pull policy allows the image to be used locally. Fireactions boots prepared Linux root filesystem images. A general-purpose Docker image is not automatically a supported guest.

## A VM does not start or reach the guest agent

Read the Fireactions service journal and inspect the configured kernel and Firecracker executable paths. Check that KVM is available, the configured kernel is readable, the guest image follows the boot contract, and the guest agent service starts. Run host validation again after you correct a prerequisite.

Use these commands to inspect the host service and the VM's agent log:

```sh
sudo journalctl -u fireactions.service
fireactions ps
fireactions logs VM_ID
```

The agent log is not an interactive session. Fireactions does not support guest SSH login, a default password, stdin, or PTY.

## containerd cannot create a devmapper snapshot

Check that the configured containerd daemon has the devmapper snapshotter loaded and that it can create snapshots. Use the configuration and maintenance procedure for the containerd installation on this host. Do not overwrite containerd configuration or stop its service as a generic repair. Do not remove namespaces, images, snapshots, or storage devices to clear an error. Those resources can belong to other workloads.

## A VM remains after the service stops

The independent `fireactions-reaper.timer` cleans expired VMs and VMs from a daemon that no longer owns them. Check the timer and service journal:

```sh
sudo systemctl status fireactions-reaper.timer
sudo journalctl -u fireactions-reaper.service
```

You can run the cleanup command with the service configuration:

```sh
sudo fireactions reap --config /etc/fireactions/config.yaml
```

Do not remove quarantined journal records by hand. They identify resources that an operator must assess. The reaper uses saved process identities and resource ownership labels. It does not treat a matching process name as proof that a resource belongs to Fireactions.

## A shell probe reports a missing directory

Runner 13.2 can probe the repository directory before a workflow without checkout declares `FORGEJO_WORKSPACE`. That early probe reports `plugin exec: open workdir: no such file or directory`, and Runner selects its normal shell fallback. Fireactions prepares the declared workspace before the actual workflow script runs. An explicit missing working directory still causes a process launch failure.

## A workflow requests an unsupported feature

The plugin is pinned to Runner 13.2 alpha and supports Linux guests only. It does not support service containers, Docker-container actions, stdin, PTY, or signal RPC. Capability adjustments are advisory and ignored. Use actions and workflow steps that run commands in the supported Linux guest.
