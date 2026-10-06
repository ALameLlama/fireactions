# Run your first build

After you install Fireactions and register Forgejo Runner on the host, run a workflow that requests a configured profile label. A profile label uses the form `firecracker:firecracker://PROFILE`, where `PROFILE` is a name in the Fireactions host configuration.

## Check the host services

Make sure the main host service is running and the independent reaper timer is enabled:

```bash
sudo systemctl status fireactions.service
sudo systemctl status fireactions-reaper.timer
fireactions pools list
```

The supplied configuration keeps one clean idle VM for `ubuntu-24.04` and zero for `ubuntu-24.04-large`. If you need another idle VM, change the configured target with:

```bash
fireactions pools scale ubuntu-24.04 --replicas 2
```

A ready idle VM has never run a job. When a job claims it, Fireactions destroys that VM at the end of its lease and creates a clean replacement to restore the idle target. It does not reuse a VM or resume a job after a service restart.

## Create a Forgejo workflow

Create `.forgejo/workflows/fireactions.yml` in a Forgejo repository. Replace `ubuntu-24.04` with the exact pool name configured on your host.

```yaml
name: Fireactions test
on:
  workflow_dispatch:
jobs:
  test:
    runs-on: firecracker:firecracker://ubuntu-24.04
    steps:
      - name: Check the guest
        run: |
          id
          uname -a
          test -d /workspace
```

The guest image creates the `ci` user and workspace. Configure the profile to use `default_user: ci` so workflow commands run with that account.

## Run and observe the job

Start the workflow from the Forgejo Actions page. Forgejo Runner must run on the host and connect to `unix:///run/fireactions/plugin.sock`. The guest network must reach Forgejo when the workflow checks out a repository, and it must reach any other services used by the job.

Use these commands on the host to inspect Fireactions:

```bash
fireactions pools list
fireactions ps
sudo journalctl -u fireactions.service -f
```

Fireactions supports Forgejo Runner protocol 13.2 on Linux. It does not support stdin, PTY, or signal RPC, service containers, or Docker-container actions. Do not use a workflow that depends on those features.

A timeout is a hard lease limit. Provisioning time counts toward the lease. At expiry Fireactions stops the VM, and cleanup grace applies only to resource cleanup. The reaper timer handles expired resources after a daemon crash.
