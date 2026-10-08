# Command-line reference

The `fireactions` command starts the host server, validates configuration, and manages configured profiles and guest VMs. Run `fireactions --help` or `fireactions COMMAND --help` for command help.

## Server and agent

`fireactions server` starts the host plugin server. It reads `/etc/fireactions/config.yaml` by default. Use `--config` or `-f` to select another file.

```sh
sudo fireactions server --config /etc/fireactions/config.yaml
```

`fireactions agent` starts the guest agent. The guest image starts this command as a service. Its flags are `--log-level` or `-l`, `--port` (default `9001`), `--workspace-root` (default `/workspace`), `--default-user` (default `ci`), `--max-transfer-bytes`, and `--max-archive-entries`. The server and agent use the Fireactions guest protocol. The current Forgejo runner plugin protocol is pinned to Runner 13.2 alpha and supports Linux guests only.

## Validate configuration

The positional form checks the configuration schema without checking host resources:

```sh
fireactions validate /etc/fireactions/config.yaml
```

The host form also checks configured Firecracker and kernel paths, KVM, containerd, the devmapper snapshotter, CNI plugins, and host resolver prerequisites. It does not start the server or create VMs or host resources.

```sh
sudo fireactions validate --host /etc/fireactions/config.yaml
```

To read the configured socket group, validate the file and print only the group name:

```sh
fireactions validate --print-socket-group /etc/fireactions/config.yaml
```

This form uses the YAML parser and validates the whole configuration. On success, standard output contains one group name and a newline. On failure, it returns a nonzero exit status without printing a group. It does not require the group to exist. If you also pass `--host`, host validation must succeed before it prints the group.

## Reap expired or abandoned VMs

`reap` is a standalone cleanup command. It does not require a running plugin. Use `--config` to select its configuration file. The default is `/etc/fireactions/config.yaml`.

```sh
sudo fireactions reap --config /etc/fireactions/config.yaml
```

The reaper preserves unexpired VMs owned by a live daemon. It uses saved process identities, CNI configuration, and containerd ownership labels. Journal records are under `state_dir/journal`. Corrupt records are quarantined with a `.json.corrupt-<id>` suffix. They are evidence for an operator to assess. Do not remove them without determining whether their resources still exist. Startup cleans old VMs instead of resuming jobs. The independent `fireactions-reaper.timer` must stay enabled when the main service stops or crashes.

## Plugin endpoint

Management commands use the local Unix socket `unix:///run/fireactions/plugin.sock` by default. They accept `--endpoint` or `-e` to select another endpoint. Fireactions does not expose a TCP gRPC endpoint.

```sh
fireactions -e unix:///run/fireactions/plugin.sock pools list
```

## Profile commands

`pools list` (alias `pools ls`) lists configured profiles and their status. Its `current` count is the number of ready idle VMs, not active environments.

```sh
fireactions pools list
```

`pools pause NAME` stops idle replenishment and cold acquisition. Existing idle VMs can still be claimed. `pools resume NAME` resumes replenishment and cold acquisition.

```sh
fireactions pools pause ubuntu-24.04
fireactions pools resume ubuntu-24.04
```

`pools scale NAME --replicas N` changes the target number of clean idle VMs. The count can be zero. Scaling down removes idle VMs and cancels excess idle provisioning. It does not stop claimed environments.

```sh
fireactions pools scale ubuntu-24.04 --replicas 3
```

## VM and image commands

`ps` (alias `ls`) lists machines across profiles. `logs MACHINE_ID` reads guest agent logs. Add `--follow` or `-f` to stream logs. Add `--tail N` to limit the initial output. A zero tail value returns all buffered lines.

```sh
fireactions ps
fireactions logs VM_ID --follow --tail 50
```

`image list` (alias `image ls`) lists container images managed through the server. `image remove NAME` (alias `image rm`) removes an image. Image removal can affect a configured profile. Do not remove an image that a running server needs.

```sh
fireactions image list
fireactions image remove localhost/fireactions-guest:ubuntu-24.04
```

`version` prints the Fireactions version. `completion SHELL` generates completion code for a shell.

## Runner and guest limits

The plugin accepts a configured profile name, not an arbitrary Docker image. It resolves profile selection in this order: request image, `label_arg`, then the `profile` backend option. If no value selects a configured profile, the request fails. There is no implicit default profile.

The plugin rejects service containers and Docker-container actions. It does not provide stdin, PTY, or signal RPC support. Linux guests are supported. Capability adjustments are advisory and ignored. A claimed VM is disposable. Fireactions does not reuse it for another job or resume a job after restart. Fireactions does not add Firecracker jailer isolation.
