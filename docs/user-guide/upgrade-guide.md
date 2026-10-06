# Upgrade Fireactions

Use a Fireactions binary built from the intended source revision. GitHub hosts the source repository and release files, but the host must run the fork-built Fireactions binary. Do not install an upstream runner product in its place.

A host service restart destroys old claimed and idle VMs before Fireactions accepts jobs. Jobs do not resume after a restart. Schedule an upgrade when interrupting active jobs is acceptable.

## Replace the binary

Copy the new binary to the host. Make sure it is executable and keep a backup of the current binary outside the active install path.

Stop the host service before replacing the binary:

```bash
sudo systemctl stop fireactions.service
```

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

Start the service and confirm that the independent reaper timer remains enabled:

```bash
sudo systemctl daemon-reload
sudo systemctl start fireactions.service
sudo systemctl enable --now fireactions-reaper.timer
sudo systemctl status fireactions.service
sudo systemctl status fireactions-reaper.timer
```

The reaper timer must stay enabled even when the main service is stopped or crashes. It cleans expired or abandoned resources independently.

## Verify the upgrade

Check the installed binary version, pool state, and host service journal:

```bash
fireactions version
fireactions pools list
sudo journalctl -u fireactions.service -n 100
```

Run a Forgejo workflow with a configured Fireactions profile label after the host service is ready. Fireactions supports Forgejo Runner protocol 13.2 on Linux. Keep the Runner and its registration token on the host.
