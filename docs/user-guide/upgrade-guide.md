# Upgrade Fireactions

Use a Fireactions binary built from the intended source revision. GitHub hosts the source repository and release files, but the host must run the fork-built Fireactions binary. Do not install an upstream runner product in its place.

On NixOS, use the native commands below. On other Linux hosts, stop active workflows before restarting the backend. A backend restart destroys its guests, and jobs cannot resume afterward.

## Update on NixOS

From the directory that contains the host flake, run:

```bash
nix flake update
sudo nixos-rebuild switch --flake .
```

If the configuration name differs from the host name, append `#CONFIGURATION_NAME` to the flake argument.
The default module builds the host binary and matching Ubuntu 24.04 guest archive together.
Systemd drains Runner before stopping the backend and imports the new image before restarting them.
No Docker build, archive upload, or separate maintenance script is required.
To apply only configuration changes, omit `nix flake update`.
NixOS rollback restores the previous system and its matching archive.

The module cannot rebuild an external archive selected through a runtime filename.
If you override `imageArchive` with that filename or `null`, keep its guest binary matched to the host yourself.
The same rule applies to runtime filenames under `pools.<name>.imageArchive`.
For additional derived archives, override their `fireactions` argument with `config.services.fireactions.package`.
After replacing a runtime archive, restart `fireactions-image-import.service`.
The module no longer watches external files for changes.

## Update on other Linux hosts

Rebuild each guest image with the new binary from the same source revision.
Replacing only the host binary leaves the old guest agent in each VM.
See [Guest images](images.md) for the build and export commands.

## Replace the binary

Copy the new binary to the host. Make sure it is executable and keep a backup of the current binary outside the active install path.

Stop the host service, reaper timer, and reaper service before replacing the binary:

```bash
sudo systemctl stop fireactions-reaper.timer fireactions.service fireactions-reaper.service
```

Import the rebuilt guest archive into the configured containerd namespace before restarting Fireactions. The example uses the `fireactions` namespace:

```bash
sudo ctr --namespace fireactions images import --local --snapshotter devmapper fireactions-guest.tar
```

Keep each profile's `image` reference aligned with the imported image.

Install the new binary at the existing Fireactions binary path. The default installer uses `/usr/local/bin/fireactions`:

```bash
sudo install -m 0755 ./fireactions /usr/local/bin/fireactions
```

Validate the current configuration, including existing host prerequisites:

```bash
sudo /usr/local/bin/fireactions validate /etc/fireactions/config.yaml
sudo /usr/local/bin/fireactions validate --host /etc/fireactions/config.yaml
```

Review release notes and update the configuration when the new version requires a schema change. Do not replace the host state directory or delete journal records as part of an upgrade.

Journal locks now use one `.metadata.lock` file. Do not run old and new Fireactions binaries against the same state directory together.

Stop every daemon and reaper that uses this state directory, including manually started processes. After they stop, you can remove old per-VM lock files:

```bash
sudo find /var/lib/fireactions/journal -maxdepth 1 -type f -name '*.lock' ! -name '.metadata.lock' -delete
```

If you use a custom `state_dir`, change the path in this command. Keep `.metadata.lock` and all JSON journal records.

Start the service and confirm that the independent reaper timer remains enabled:

```bash
sudo systemctl daemon-reload
sudo systemctl start fireactions.service
sudo systemctl enable --now fireactions-reaper.timer
sudo systemctl status fireactions.service
sudo systemctl status fireactions-reaper.timer
```

Keep the reaper timer enabled during normal operation, including main-service stops and crashes. Stop it temporarily when replacing the binary.

## Verify the upgrade

Check the installed binary version, pool state, and host service journal:

```bash
fireactions version
fireactions pools list
sudo journalctl -u fireactions.service -n 100
```

Run a Forgejo workflow with a configured Fireactions profile label after the host service is ready. Fireactions supports Forgejo Runner protocol 13.2 on Linux. Keep the Runner and its registration token on the host.
