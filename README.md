# shoes-microsandbox

A [myshoes](https://github.com/whywaita/myshoes) provider that creates ephemeral VMs using [microsandbox](https://github.com/superradcompany/microsandbox) — lightweight, hardware-isolated microVMs for GitHub Actions self-hosted runners.

Each `AddInstance` call boots a fresh microsandbox VM, runs the GitHub Actions runner setup script inside it, and returns the VM's IP address. When the job finishes, `DeleteInstance` stops and removes the VM completely — no leftover state.

## Architecture

```
┌─────────────────────────────────────────────┐
│                  myshoes                     │
│            (Orchestrator / Scheduler)        │
└──────────────────┬──────────────────────────┘
                   │ gRPC (hashicorp go-plugin)
                   │ AddInstance / DeleteInstance
┌──────────────────▼──────────────────────────┐
│         shoes-microsandbox                    │
│   ┌─────────────────────────────────────┐    │
│   │  AddInstance:                       │    │
│   │    1. CreateSandbox(image, mem, cpu)│    │
│   │    2. Exec(setup_script)            │    │
│   │    3. Detach (VM keeps running)    │    │
│   │    4. Return cloud_id + IP          │    │
│   │                                     │    │
│   │  DeleteInstance:                    │    │
│   │    1. GetSandbox(cloud_id)          │    │
│   │    2. Stop → Remove                 │    │
│   └─────────────────────────────────────┘    │
└──────────────────┬──────────────────────────┘
                   │ microsandbox SDK (embedded FFI)
                   │
┌──────────────────▼──────────────────────────┐
│           microsandbox runtime               │
│     (libkrunfw + msb, auto-downloaded)       │
└──────────────────┬──────────────────────────┘
                   │ microVM (KVM / Apple Silicon)
┌──────────────────▼──────────────────────────┐
│           GitHub Actions Runner               │
│      (ephemeral VM — isolated per job)        │
└───────────────────────────────────────────────┘
```

## Prerequisites

- **Linux** with KVM enabled (`/dev/kvm` available), **or** macOS with Apple Silicon
- Go 1.24+
- The `msb` runtime and `libkrunfw` are auto-downloaded on first run via `microsandbox.EnsureInstalled()`

## Quick Start

### Running with the default image

shoes-microsandbox uses the official [GitHub Actions Runner container image](https://github.com/actions/runner/pkgs/container/actions-runner) (`ghcr.io/actions/actions-runner`) as the default VM image. This image contains the runner agent and all dependencies needed to connect to GitHub Actions.

```bash
# Build the plugin
go build -o shoes-microsandbox .

# Set environment variables for myshoes
export SHOES_PLUGIN_PATH=/path/to/shoes-microsandbox

# (Optional) Pin a specific runner version
export MSB_IMAGE=ghcr.io/actions/actions-runner:2.334.0

# Start myshoes
myshoes server
```

### Using a custom image

You can use any OCI-compatible image as the runner VM. For private registries, set the authentication credentials:

```bash
# Private image from GHCR
export MSB_IMAGE=ghcr.io/myorg/custom-runner:latest
export MSB_REGISTRY_USERNAME=myuser
export MSB_REGISTRY_PASSWORD=ghp_xxxxxxxxxxxx

# Or from Docker Hub
export MSB_IMAGE=docker.io/myorg/runner:latest
export MSB_REGISTRY_USERNAME=myuser
export MSB_REGISTRY_PASSWORD=dckr_pat_xxxxxxxxxxxx
```

### Runner versions

The `ghcr.io/actions/actions-runner` image is tagged by runner version. Available tags include:

| Tag | Description |
|---|---|
| `latest` | Latest stable runner release |
| `2.334.0` | Specific runner version |
| `2.333.1` | Previous runner version |

You can pin a version to avoid unexpected updates:

```bash
export MSB_IMAGE=ghcr.io/actions/actions-runner:2.334.0
```

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|---|---|---|
| `MSB_IMAGE` | `ghcr.io/actions/actions-runner:latest` | OCI image for the runner VM |
| `MSB_MEMORY_MIB` | `2048` | Memory per sandbox (MiB) |
| `MSB_CPUS` | `2` | CPU cores per sandbox |
| `MSB_RUNNER_USER` | `runner` | User inside the VM for script execution |
| `MSB_REGISTRY_USERNAME` | *(empty)* | Private registry username for pulling images |
| `MSB_REGISTRY_PASSWORD` | *(empty)* | Private registry password for pulling images |
| `MSB_PULL_POLICY` | `""` (if-missing) | Image pull policy: `always`, `if-missing`, or `never` |
| `SHOES_PLUGIN_MAGIC_COOKIE` | *(set by myshoes)* | Plugin handshake cookie |
| `SHOES_PLUGIN_PATH` | *(set by myshoes)* | Path to this binary |

### Resource Type Mapping

myshoes `ResourceType` values map to sandbox resources as follows:

| ResourceType | Memory (MiB) | CPUs |
|---|---|---|
| Nano | 512 | 1 |
| Micro | 1024 | 1 |
| Small | 2048 | 2 |
| Medium | 4096 | 2 |
| Large | 8192 | 4 |
| XLarge | 16384 | 4 |
| XLarge2 | 32768 | 8 |
| XLarge3 | 65536 | 8 |
| XLarge4 | 131072 | 16 |

## Building

```bash
go build -o shoes-microsandbox .
```

## Usage with myshoes

1. Build the binary:
   ```bash
   go build -o shoes-microsandbox .
   chmod +x shoes-microsandbox
   ```

2. Configure myshoes to use this plugin:
   ```bash
   export SHOES_PLUGIN_PATH=/path/to/shoes-microsandbox
   ```
   Or point to a URL:
   ```bash
   export SHOES_PLUGIN_PATH=https://releases.example.com/shoes-microsandbox
   ```

3. Start myshoes server:
   ```bash
   myshoes server
   ```

4. Create a target via the myshoes API pointing to your GitHub repository/organization.

## Docker

```dockerfile
FROM golang:1.24 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o shoes-microsandbox .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/shoes-microsandbox /usr/local/bin/shoes-microsandbox
ENTRYPOINT ["shoes-microsandbox"]
```

## How It Works

### AddInstance

1. Receives `runner_name` and `setup_script` from myshoes
2. Calculates sandbox resources from `ResourceType`
3. Creates a microsandbox VM with `CreateSandbox()`:
   - Uses the configured OCI image (default: `ghcr.io/actions/actions-runner:latest`)
   - Applies registry credentials if configured
   - Applies pull policy (default: `if-missing`)
   - Sets hostname to `myshoes-<runner_name>`
   - `WithReplace()` ensures no conflicts with existing sandboxes
4. Executes the GitHub Actions runner setup script inside the VM
5. Retrieves the VM's IP address
6. Detaches from the VM (it continues running independently)
7. Returns `cloud_id`, `ip_address`, and `shoes_type="shoes-microsandbox"`

### DeleteInstance

1. Looks up the sandbox by `cloud_id`
2. Stops the VM gracefully (fallback: kill)
3. Removes the sandbox's persisted state
4. The VM is completely gone — no leftover resources

## Security

- Each GitHub Actions job runs in a **hardware-isolated microVM** (not a shared container)
- The VM is destroyed after the job, ensuring clean state
- microsandbox's secret injection and network policy features are available for additional hardening
- No root daemon required — the SDK embeds the runtime directly

## Limitations

- **KVM required on Linux** — will not work on environments without `/dev/kvm`
- Currently Linux (x86_64/ARM64) and macOS (Apple Silicon) only
- The `ip_address` may be an internal NAT address depending on your microsandbox network configuration
- Beta software — expect breaking changes in microsandbox v0.x

## License

Apache-2.0