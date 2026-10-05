# Interacting with Fireactions via CLI

Fireactions provides a CLI for interacting with the server.

```bash
$ fireactions --help
Run Forgejo workflows in disposable Firecracker virtual machines.

Usage:
  fireactions [command]

Main application commands:
  server      Starts the Fireactions server
  agent       Starts the Fireactions agent
  validate    Validates the server configuration file

Pool management commands:
  pools       Manage pools

Machine management commands:
  ps          List all running machines across all pools
  logs        Stream logs from the fireactions-agent service inside a machine

Image management commands:
  image       Manage images

Additional Commands:
  version     Show version information
  help        Help about any command
  completion  Generate the autocompletion script for the specified shell

Flags:
  -h, --help      help for fireactions
  -v, --version   version for fireactions

Use "fireactions [command] --help" for more information about a command.
```

## Server Endpoint

Most commands that interact with the Fireactions server accept an `--endpoint` (or `-e`) flag.

```bash
fireactions --endpoint unix:///run/fireactions/plugin.sock pools list
# or using shorthand
fireactions -e unix:///run/fireactions/plugin.sock pools list
```

The default endpoint is `unix:///run/fireactions/plugin.sock`.

## Commands

### Main Application Commands

#### `server`

Starts the Fireactions server.

```bash
fireactions server
```

By default, the server looks for a configuration file at `/etc/fireactions/config.yaml`. You can specify a different path using the `--config` (or `-f`) flag:

```bash
fireactions server --config /path/to/config.yaml
```

#### `agent`

Starts the Fireactions agent. This command should be run inside the virtual machine and is automatically executed by the VM image.

```bash
fireactions agent
```

The agent reads metadata from the Firecracker MMDS service to configure itself. You can optionally specify the log level:

```bash
fireactions agent --log-level debug
```

Available log levels: `debug`, `info`, `warn`, `error`, `fatal`, `panic`, `trace` (default: `info`)

#### `validate`

Validates a server configuration file without starting the server. This is useful for checking configuration syntax and validating settings before deployment.

```bash
fireactions validate /path/to/config.yaml
```

### Pool Management Commands

All pool management commands accept an `--endpoint` (or `-e`) flag (default: `unix:///run/fireactions/plugin.sock`).

#### `pools list` (alias: `pools ls`)

List all configured pools with their current status. Current replicas count ready, never-claimed idle VMs, not active environments.

```bash
fireactions pools list
```

#### `pools pause <NAME>`

Pause idle provisioning and cold acquisition. Existing idle VMs remain available for claims. Claimed VMs continue to run.

```bash
fireactions pools pause ubuntu-24.04
```

#### `pools resume <NAME>`

Resume idle replenishment and cold acquisition. The pool creates clean replacements to reach its idle target.

```bash
fireactions pools resume ubuntu-24.04
```

#### `pools scale <NAME> --replicas <N>`

Set the target number of ready, never-claimed idle VMs. Scaling down removes idle VMs and cancels excess idle provisioning. It never terminates claimed environments.

```bash
# Keep five clean idle VMs
fireactions pools scale ubuntu-24.04 --replicas 5

# Remove idle capacity without stopping active environments
fireactions pools scale ubuntu-24.04 --replicas 0
```

The `--replicas` flag is required. Counts must be between 0 and 2147483647. An active pool with zero replicas creates a dedicated cold VM for each acquisition.

### Machine Management Commands

All machine management commands accept an `--endpoint` (or `-e`) flag (default: `unix:///run/fireactions/plugin.sock`).

#### `ps` (alias: `ls`)

List all running machines across all pools.

```bash
fireactions ps
```


#### `logs <MACHINE_ID>`

Stream logs from the fireactions-agent gRPC service running inside a machine. This shows the zerolog output from the agent service itself, including agent startup, status changes, and any errors from the agent.

```bash
# Show all buffered logs
fireactions logs default-abc123

# Follow logs in real-time (like tail -f)
fireactions logs default-abc123 --follow

# Show last 50 lines and follow
fireactions logs default-abc123 --follow --tail 50
```

**Flags:**
- `-f, --follow`: Follow log output (stream continuously like tail -f)
- `--tail N`: Number of lines to show from end (0 = all buffered logs)

### Image Management Commands

All image management commands accept an `--endpoint` (or `-e`) flag (default: `unix:///run/fireactions/plugin.sock`).

#### `image list` (alias: `image ls`)

List all container images managed by Fireactions.

```bash
fireactions image list
```

#### `image remove <NAME>` (alias: `image rm`)

Remove a container image from the Fireactions server.

```bash
fireactions image remove ghcr.io/myorg/myimage:latest
```

### Additional Commands

#### `version`

Show version information for Fireactions.

```bash
fireactions version
```

## Examples

### Basic Workflow

```bash
# Validate configuration before starting
fireactions validate /etc/fireactions/config.yaml

# Start the server
fireactions server --config /etc/fireactions/config.yaml

# List all pools
fireactions pools list

# Scale up a pool
fireactions pools scale production --replicas 10

# Check machine status
fireactions ps

# View logs from a specific machine
fireactions logs production-abc123 --follow


# Scale down when done
fireactions pools scale production --replicas 0

# List images
fireactions image list

# Remove an unused image
fireactions image remove ghcr.io/example/old-image:v1
```

### Using an alternate local socket

Use `--endpoint` with a `unix://` URI to select another local Unix socket. The server does not expose TCP gRPC endpoints.

```bash
fireactions -e unix:///run/fireactions/plugin.sock ps
```
