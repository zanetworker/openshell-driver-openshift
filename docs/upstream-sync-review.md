# Upstream Sync Review: openshell-driver-openshift vs NVIDIA/OpenShell

**Date**: 2026-04-27
**Upstream ref**: NVIDIA/OpenShell main (latest commit: f8fb382)
**Fork ref**: zanetworker/openshell-driver-openshift main (latest commit: 7ca6a23)

## Proto Drift

### 1. `ResolveSandboxEndpoint` RPC removed upstream (CRITICAL)

**Upstream PR**: [#867 - feat(server,sandbox): supervisor-initiated SSH connect and exec over gRPC-multiplexed relay](https://github.com/NVIDIA/OpenShell/pull/867)

Your proto includes `ResolveSandboxEndpoint`, `ResolveSandboxEndpointRequest`,
`ResolveSandboxEndpointResponse`, and `SandboxEndpoint` messages (proto lines 42-260).
Upstream removed this RPC entirely in PR #867.

**Why it existed**: The original architecture had the gateway dial sandbox pods
directly for SSH connect and exec. When a user ran `openshell sandbox connect` or
`openshell sandbox exec`, the gateway needed to know the sandbox's IP:port. So it
called `ResolveSandboxEndpoint` on the compute driver, which looked up the pod IP
(or fell back to cluster DNS) and returned an endpoint the gateway could dial. Our
implementation does exactly this: tries the pod IP via `instance_id`, falls back to
`<name>.<namespace>.svc.cluster.local:2222`.

#### Old model (your driver implements this):

```mermaid
sequenceDiagram
    participant User as openshell CLI
    participant GW as Gateway
    participant Driver as Compute Driver
    participant Pod as Sandbox Pod :2222

    User->>GW: sandbox connect / exec
    GW->>Driver: ResolveSandboxEndpoint(sandbox)
    Driver->>Driver: lookup pod IP or build DNS name
    Driver-->>GW: SandboxEndpoint{ip, port: 2222}
    GW->>Pod: TCP + TLS dial to pod_ip:2222
    Note over GW,Pod: NSSH1 HMAC handshake<br/>(ssh_handshake_secret + nonce)
    GW->>Pod: SSH session over direct connection
    Note over GW,Pod: Each connect/exec = new TCP + TLS + handshake
```

**Why it was removed**: PR #867 introduced a fundamentally different connectivity
model. Instead of the gateway dialing outward to each sandbox, the supervisor inside
the sandbox now initiates a persistent gRPC session back to the gateway
(`ConnectSupervisor`). SSH and exec traffic rides this session as multiplexed
`RelayStream` RPCs on the same HTTP/2 connection. This eliminates the need for the
gateway to resolve sandbox endpoints because the sandbox connects to the gateway,
not the other way around.

#### New model (upstream, post-PR #867):

```mermaid
sequenceDiagram
    participant Supervisor as Sandbox Supervisor
    participant GW as Gateway
    participant User as openshell CLI

    Note over Supervisor,GW: On sandbox boot (once)
    Supervisor->>GW: ConnectSupervisor (persistent bidi gRPC stream)
    Note over Supervisor,GW: Single TCP + TLS for lifetime of sandbox

    User->>GW: sandbox connect
    GW->>Supervisor: RelayOpen (over control stream)
    Supervisor->>GW: RelayStream (new HTTP/2 stream, same connection)
    Note over Supervisor,GW: SSH traffic multiplexed as RelayFrames
    Supervisor->>Supervisor: bridge to /run/openshell/ssh.sock

    User->>GW: sandbox exec
    GW->>Supervisor: RelayOpen (over control stream)
    Supervisor->>GW: RelayStream (another HTTP/2 stream)
    Note over Supervisor,GW: N concurrent sessions = 1 TCP connection
```

The benefits of this reversal:
- One TLS handshake per sandbox lifetime instead of one per connect/exec
- One TCP connection per sandbox instead of 1+N (53 TCPs in a 50-relay storm dropped to 3)
- No gateway-to-sandbox network path required (simplifies firewalls, LBs, NetworkPolicies)
- SSH daemon moved from port 2222 to a Unix socket (`/run/openshell/ssh.sock`),
  removing the NSSH1 HMAC handshake and nonce replay detection entirely
- The `ssh_handshake_secret` / `ssh_handshake_skew_secs` config fields are now dead
  code upstream (tracked for cleanup in OS-102)

**Upstream proto** (9 RPCs): GetCapabilities, ValidateSandboxCreate, GetSandbox,
ListSandboxes, CreateSandbox, StopSandbox, DeleteSandbox, WatchSandboxes

**Your proto** (10 RPCs): same + ResolveSandboxEndpoint

**Impact**: If the upstream gateway ever calls your driver over UDS, it will never
call `ResolveSandboxEndpoint`. The extra RPC is dead code. Your
`SandboxProvisioner` interface requires `ResolveEndpoint()` which adds unnecessary
complexity. Additionally, your driver still injects `OPENSHELL_SSH_LISTEN_ADDR`
(port 2222) and `OPENSHELL_SSH_HANDSHAKE_SECRET` into sandbox pods, which are no
longer used upstream.

**Action**: Remove `ResolveSandboxEndpoint` from proto, regenerate Go code, remove
from `SandboxProvisioner` interface, remove from `provisioner.go` and `driver.go`.
Also remove the `SSHListenAddr` and `SSHHandshakeSecret` config fields and their
env var injection.

**Files affected**:
- `proto/compute_driver.proto` (lines 42-44, 241-260)
- `gen/computev1/compute_driver.pb.go` (regenerate)
- `gen/computev1/compute_driver_grpc.pb.go` (regenerate)
- `internal/driver/interfaces.go` (line 17)
- `internal/driver/provisioner.go` (lines 204-228, 343-348)
- `internal/driver/driver.go` (lines 196-205)
- `internal/driver/config.go` (lines 12-13)
- `cmd/driver/main.go` (lines 35-38)


## Security Context

### 2. `privileged: true` vs granular capabilities (CRITICAL)

**Upstream PR**: [#817 - refactor(server): extract kubernetes compute driver](https://github.com/NVIDIA/OpenShell/pull/817)
(capabilities were part of the original server code, carried over during extraction)

Your driver sets `"privileged": true` and `"runAsUser": 0` on the agent container
(`provisioner.go:275-278`).

Upstream uses granular Linux capabilities instead:
```json
{
  "capabilities": {
    "add": ["SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE", "SYSLOG"]
  }
}
```

#### What the supervisor actually needs root for:

```mermaid
sequenceDiagram
    participant Sup as Supervisor (root)
    participant NS as Network Namespace
    participant Proxy as CONNECT Proxy
    participant Agent as Agent Process (non-root)

    Note over Sup: Starts as root (runAsUser: 0)
    Sup->>NS: create network namespace (SYS_ADMIN)
    Sup->>NS: create veth pair (NET_ADMIN)
    Sup->>Sup: install seccomp filter (SYS_ADMIN)
    Sup->>Sup: configure Landlock LSM (SYS_ADMIN)
    Sup->>Sup: read /dev/kmsg for bypass detection (SYSLOG)
    Sup->>Agent: drop to run_as_user / run_as_group
    Note over Agent: Runs as non-root sandbox user
    Agent->>Proxy: outbound network request
    Proxy->>Proxy: read /proc/pid/fd for binary identity (SYS_PTRACE)
    Proxy->>Proxy: enforce network policy per binary
```

**Why upstream uses these specific capabilities**:
- `SYS_ADMIN`: seccomp filter installation and network namespace creation
- `NET_ADMIN`: network namespace veth setup
- `SYS_PTRACE`: CONNECT proxy reads /proc/pid/fd to resolve binary identity for
  network policy enforcement
- `SYSLOG`: reading /dev/kmsg for bypass detection diagnostics

**Impact**: `privileged: true` grants ALL capabilities plus host device access. On
OpenShift, the `privileged` SCC is required, which is a major security escalation.
With granular capabilities, a custom SCC with just those 4 caps would suffice.

#### OpenShift SCC comparison:

```mermaid
flowchart LR
    subgraph current["Your driver (current)"]
        P[privileged: true] --> SCC1[Requires: privileged SCC]
        SCC1 --> R1["All 38+ Linux capabilities<br/>Host device access<br/>Host network optional<br/>No SELinux confinement"]
    end

    subgraph target["Upstream (target)"]
        C["capabilities.add:<br/>SYS_ADMIN, NET_ADMIN,<br/>SYS_PTRACE, SYSLOG"] --> SCC2[Requires: custom SCC<br/>with 4 caps only]
        SCC2 --> R2["4 specific capabilities<br/>No host device access<br/>SELinux still enforced<br/>Seccomp still enforced"]
    end

    style current fill:#fee,stroke:#c00
    style target fill:#efe,stroke:#0a0
```

**Action**: Replace privileged security context with capabilities list. Keep
`runAsUser: 0` (the supervisor needs root, then drops privileges for child
processes).

**Files affected**:
- `internal/driver/provisioner.go` (lines 275-278)


## Supervisor Side-Loading

### 3. Init container (yours) vs hostPath (upstream) (INTENTIONAL DIVERGENCE)

**Upstream PR**: [#267 - refactor(sandbox): sandboxes are managed as separate community images](https://github.com/NVIDIA/OpenShell/pull/267)

Upstream (PR #267, merged 2026-03-13) replaced init containers with hostPath volumes.
The supervisor binary is baked into the k3s cluster node image and mounted read-only
via hostPath at `/opt/openshell/bin`.

Your driver uses an init container that copies the supervisor from a container image
(`quay.io/azaalouk/openshell-supervisor:latest`) into an emptyDir shared volume.

#### Upstream (k3s hostPath):

```mermaid
sequenceDiagram
    participant Node as k3s Node
    participant Pod as Sandbox Pod

    Note over Node: Supervisor binary baked into<br/>node image at /opt/openshell/bin
    Pod->>Node: mount hostPath /opt/openshell/bin (read-only)
    Pod->>Pod: run /opt/openshell/bin/openshell-sandbox
    Note over Pod: No init container needed<br/>No image pull for supervisor
```

#### Your driver (OpenShift init container):

```mermaid
sequenceDiagram
    participant Reg as Container Registry
    participant Init as Init Container
    participant Vol as emptyDir Volume
    participant Agent as Agent Container

    Init->>Reg: pull supervisor image
    Init->>Vol: cp /usr/local/bin/openshell-sandbox to shared volume
    Note over Init: Init container exits
    Agent->>Vol: mount shared volume (read-only)
    Agent->>Agent: run /opt/openshell/bin/openshell-sandbox
    Note over Agent: Extra image pull + copy step,<br/>but works without node access
```

**This divergence is correct for OpenShift.** HostPath volumes are heavily restricted
by OpenShift SCCs (requires `privileged` or a custom SCC with `allowHostDirVolumePlugin`).
The init-container approach works without any node filesystem assumptions and only
needs standard volume permissions. No action needed here.


## Missing Features

### 4. Workspace persistence (PVC) (IMPORTANT)

**Upstream PR**: [#739 - fix(bootstrap,server): persist sandbox state across gateway stop/start cycles](https://github.com/NVIDIA/OpenShell/pull/739)

Upstream added PVC-backed workspace persistence so sandbox data survives pod
rescheduling. Every sandbox gets:
- A `volumeClaimTemplates` entry (2Gi ReadWriteOnce)
- An init container that seeds the PVC from the image's `/sandbox` on first use
- A sentinel file `.workspace-initialized` to skip re-seeding

Your driver has no workspace persistence. Sandbox data is lost on pod restart.

#### Upstream workspace persistence flow:

```mermaid
sequenceDiagram
    participant CRD as Sandbox CRD Controller
    participant PVC as PVC (2Gi RWO)
    participant Init as workspace-init Container
    participant Agent as Agent Container

    CRD->>PVC: create PVC from volumeClaimTemplate
    Init->>Init: check for .workspace-initialized sentinel
    alt First boot (no sentinel)
        Init->>Init: tar -cf /sandbox | tar -xpf /workspace-pvc
        Init->>PVC: write .workspace-initialized sentinel
        Note over Init: Image contents seeded into PVC
    else Subsequent boot (sentinel exists)
        Init->>Init: skip copy
        Note over Init: Instant start
    end
    Note over Init: Init container exits
    Agent->>PVC: mount PVC at /sandbox
    Note over Agent: User files, packages, dotfiles<br/>all persist across pod restarts
```

#### Your driver (no persistence):

```mermaid
sequenceDiagram
    participant Agent as Agent Container
    participant FS as Container Filesystem

    Agent->>FS: write files to /sandbox (ephemeral)
    Note over FS: Pod restart or reschedule
    FS--xAgent: all data lost
    Note over Agent: User must reinstall packages,<br/>recreate files from scratch
```

**Upstream code**: `apply_workspace_persistence()` and
`default_workspace_volume_claim_templates()` in `driver.rs`

**Action**: Implement PVC workspace support, or document as a known limitation.


### 5. mTLS support (MODERATE)

**Upstream PR**: [#862 - feat(sandbox): load system CA certificates for upstream TLS connections](https://github.com/NVIDIA/OpenShell/pull/862)
(TLS secret volume mount was part of the k8s driver extraction in [#817](https://github.com/NVIDIA/OpenShell/pull/817))

Upstream injects a `client_tls_secret_name` volume from a Kubernetes Secret for
mTLS between sandbox and gateway. Volume is mounted at `/etc/openshell-tls/client`
with mode 0400. Your driver has no TLS support.

**Action**: Add `ClientTLSSecretName` to Config and inject the volume mount when set.


### 6. Platform event correlation in Watch (MODERATE)

**Upstream PR**: [#817 - refactor(server): extract kubernetes compute driver](https://github.com/NVIDIA/OpenShell/pull/817)

Upstream's `watch_sandboxes` watches both Sandbox CRs AND Kubernetes Events, then
correlates events to sandbox IDs using name/pod indexes. It emits
`WatchSandboxesPlatformEvent` for correlated K8s events (e.g., pod scheduling
failures, image pull errors).

Your Watch only watches Sandbox CRs and emits Updated/Deleted events. The gateway
won't see platform-level events like `FailedScheduling` or `ErrImagePull`.

#### Upstream dual-stream watch:

```mermaid
sequenceDiagram
    participant SandboxW as Sandbox CR Watcher
    participant EventW as K8s Event Watcher
    participant Index as Name/Pod Index
    participant Stream as gRPC WatchSandboxes Stream

    par Watch sandbox CRs
        SandboxW->>Index: Applied: update sandbox_name->id, pod->id
        SandboxW->>Stream: WatchSandboxesSandboxEvent
    and Watch K8s Events
        EventW->>EventW: receive Event for Pod "sandbox-abc-agent"
        EventW->>Index: lookup pod name -> sandbox_id
        alt Event correlates to a sandbox
            EventW->>Stream: WatchSandboxesPlatformEvent<br/>(reason: FailedScheduling, ErrImagePull, etc.)
        else No correlation
            EventW->>EventW: discard
        end
    end
    Note over Stream: Gateway sees both sandbox<br/>state AND platform diagnostics
```

#### Your driver (single-stream watch):

```mermaid
sequenceDiagram
    participant SandboxW as Sandbox CR Watcher
    participant Stream as gRPC WatchSandboxes Stream

    SandboxW->>Stream: WatchSandboxesSandboxEvent (Added/Modified)
    SandboxW->>Stream: WatchSandboxesDeletedEvent (Deleted)
    Note over Stream: No platform events<br/>(FailedScheduling, ErrImagePull<br/>are invisible to the gateway)
```

**Action**: Add K8s Event watching and correlation. This is important for
observability, especially on OpenShift where scheduling failures are common
due to SCC restrictions, resource quotas, and node selectors.


### 7. `host_gateway_ip` config (MODERATE)

Upstream's config includes `host_gateway_ip` for network routing. Your config
doesn't have this.

**Action**: Add to Config if needed for your deployment topology.


### 8. `image_pull_policy` config (MINOR)

Upstream passes `image_pull_policy` through to all containers (agent and init).
Your driver hardcodes no pull policy, defaulting to Kubernetes's standard behavior
(Always for :latest, IfNotPresent otherwise).

**Action**: Add `ImagePullPolicy` to Config.


## Behavioral Differences

### 9. `StopSandbox` implementation (MINOR)

**Upstream PR**: [#817 - refactor(server): extract kubernetes compute driver](https://github.com/NVIDIA/OpenShell/pull/817)

Your driver delegates `StopSandbox` to `Delete` (deletes the CR). Upstream returns
`Status::unimplemented` since stopping without deleting is not supported by the
Kubernetes driver.

**Action**: Return `codes.Unimplemented` instead of delegating to Delete.

**Files affected**:
- `internal/driver/driver.go` (lines 171-180)


### 10. API call timeouts (MINOR)

**Upstream PR**: [#907 - fix(k8s-driver): use dedicated kube client without read_timeout for watches](https://github.com/NVIDIA/OpenShell/pull/907)

Upstream wraps every K8s API call in a 30-second `tokio::time::timeout` and uses a
dedicated kube client without `read_timeout` for long-lived watch streams. Your
driver relies on gRPC context deadlines without explicit K8s API timeouts.

**Action**: Consider adding `context.WithTimeout` wrappers around K8s API calls in
the provisioner.


## Hardcoded Values to Review

| Value | Location | Issue |
|-------|----------|-------|
| `quay.io/azaalouk/openshell-supervisor:latest` | `config.go:22` | Personal image registry |
| `openshell-system` | `config.go:20` | Default namespace, upstream is configurable |
| `OPENSHELL_SANDBOX_COMMAND=sleep infinity` | `provisioner.go:337` | Not set upstream |
| `kagenti.io/type: agent` label | `provisioner.go:28,78` | Not present upstream |
| `serviceAccountName: openshell-sandbox` | `provisioner.go:295` | Hardcoded, should be configurable |
| `ANTHROPIC_BASE_URL=https://inference.local/v1` | `provisioner.go:355` | Your addition for inference routing |
| `OPENAI_BASE_URL=https://inference.local/v1` | `provisioner.go:356` | Your addition for inference routing |


## Appendix: Full Connection Architecture (Upstream Post-PR #867)

This section explains how a user's terminal ends up connected to a process
inside a sandbox pod, end to end.

### The three planes

The new architecture separates concerns into three planes, all multiplexed
over a single TCP+TLS connection per sandbox:

```mermaid
flowchart TB
    subgraph single["Single TCP + TLS + HTTP/2 Connection"]
        CS["ConnectSupervisor<br/>(bidi gRPC stream)"]
        RS1["RelayStream #1<br/>(bidi gRPC stream)"]
        RS2["RelayStream #2<br/>(bidi gRPC stream)"]
        RSN["RelayStream #N<br/>(bidi gRPC stream)"]
    end

    CS --- |"Control plane:<br/>hello, heartbeat,<br/>RelayOpen, RelayClose"| CP[Session Lifecycle]
    RS1 --- |"Data plane:<br/>raw SSH bytes"| D1["SSH session #1"]
    RS2 --- |"Data plane:<br/>raw SSH bytes"| D2["SSH session #2"]
    RSN --- |"Data plane:<br/>raw SSH bytes"| DN["SSH session #N"]

    style single fill:#f0f0ff,stroke:#339
```

### End-to-end: `openshell sandbox connect`

```mermaid
sequenceDiagram
    participant Term as User Terminal
    participant CLI as openshell CLI
    participant GW as Gateway (gRPC + HTTP)
    participant Reg as Session Registry
    participant Sup as Supervisor (in sandbox pod)
    participant SSH as SSH Daemon<br/>(Unix socket)
    participant Shell as bash/zsh<br/>(in sandbox)

    Note over Sup,GW: Sandbox boot (happens once, before any connect)
    Sup->>GW: ConnectSupervisor(SupervisorHello{sandbox_id})
    GW->>Sup: SessionAccepted{session_id, heartbeat_interval}
    Note over Sup,GW: Persistent bidi stream stays open<br/>Heartbeats keep it alive

    Note over Term,Shell: User runs: openshell sandbox connect -n my-sandbox
    CLI->>GW: CreateSshSession(sandbox_id)
    GW->>GW: generate short-lived token + ssh config
    GW-->>CLI: CreateSshSessionResponse{token, gateway_host, connect_path}
    CLI->>CLI: write ~/.ssh/openshell-config with ProxyCommand
    CLI->>Term: exec ssh -F ~/.ssh/openshell-config sandbox-alias

    Note over Term,GW: OpenSSH executes ProxyCommand
    Term->>GW: HTTP CONNECT /connect/ssh<br/>headers: x-sandbox-id, x-sandbox-token
    GW->>GW: validate token, check sandbox phase=Ready
    GW->>GW: enforce connection limits (3/token, 20/sandbox)

    GW->>Reg: open_relay(sandbox_id) -> (channel_id, relay_rx)
    GW->>Sup: RelayOpen{channel_id} (over control stream)

    Sup->>GW: RelayStream (new HTTP/2 stream)
    Sup->>GW: RelayFrame{init: RelayInit{channel_id}}
    GW->>Reg: match channel_id -> pair relay_rx with stream

    Sup->>Sup: RelayOpenResult{success: true} (over control stream)

    Note over Term,Shell: Relay established, bridge active
    GW->>GW: HTTP upgrade complete, bidirectional copy starts
    Sup->>SSH: connect to /run/openshell/ssh.sock

    Term->>GW: SSH handshake bytes
    GW->>Sup: RelayFrame{data: bytes}
    Sup->>SSH: forward to Unix socket
    SSH->>Shell: spawn shell
    Shell-->>SSH: shell output
    SSH-->>Sup: response bytes
    Sup-->>GW: RelayFrame{data: bytes}
    GW-->>Term: SSH response bytes

    Note over Term,Shell: Interactive session active<br/>All traffic is raw SSH over RelayFrames
```

### End-to-end: `openshell sandbox exec`

```mermaid
sequenceDiagram
    participant CLI as openshell CLI
    participant GW as Gateway
    participant Reg as Session Registry
    participant Sup as Supervisor
    participant SSH as SSH Daemon<br/>(Unix socket)
    participant Proc as Command Process

    Note over Sup,GW: ConnectSupervisor already established

    CLI->>GW: ExecSandbox(sandbox_id, command, workdir, env)
    GW->>Reg: open_relay(sandbox_id, timeout=15s)
    GW->>Sup: RelayOpen{channel_id} (over control stream)

    Sup->>GW: RelayStream (new HTTP/2 stream)
    Sup->>GW: RelayFrame{init: RelayInit{channel_id}}

    Sup->>SSH: connect to /run/openshell/ssh.sock
    Note over GW,SSH: SSH session opened for exec

    GW->>Sup: command + env via SSH exec channel
    Sup->>SSH: forward exec request
    SSH->>Proc: spawn command
    Proc-->>SSH: stdout/stderr chunks
    SSH-->>Sup: output bytes
    Sup-->>GW: RelayFrame{data: bytes}
    GW-->>CLI: stream ExecSandboxEvent{stdout/stderr/exit_code}

    Proc->>Proc: command exits
    Sup->>GW: RelayClose{channel_id}
    Note over GW: Relay slot freed
```

### Why this matters for the OpenShift driver

The compute driver (your code) is not involved in any of this connection flow.
The driver's job ends after `CreateSandbox` and `WatchSandboxes`. The connection
path is entirely between the supervisor (running inside the sandbox pod) and the
gateway. This is why `ResolveSandboxEndpoint` was removed from the driver proto:
the driver never needs to tell the gateway how to reach the sandbox, because the
sandbox reaches the gateway instead.

```mermaid
flowchart LR
    subgraph driver["Compute Driver (your code)"]
        CR[CreateSandbox]
        WA[WatchSandboxes]
        DE[DeleteSandbox]
    end

    subgraph gateway["Gateway"]
        GW[Sandbox Lifecycle]
        SS[Supervisor Sessions]
        ST[SSH Tunnel Handler]
    end

    subgraph pod["Sandbox Pod"]
        SV[Supervisor]
        AG[SSH Daemon]
    end

    CR --> |"gRPC over UDS"| GW
    WA --> |"gRPC stream"| GW
    SV --> |"ConnectSupervisor<br/>+ RelayStream"| SS
    ST --> |"relay bridge"| SS

    style driver fill:#efe,stroke:#0a0
    style gateway fill:#f0f0ff,stroke:#339
    style pod fill:#fff0e0,stroke:#c90
```


## Priority Order

1. **Proto sync**: Remove `ResolveSandboxEndpoint` and regenerate (breaking change, dead code)
2. **Security context**: Switch from `privileged: true` to granular capabilities
3. **StopSandbox**: Return Unimplemented instead of Delete
4. **Workspace persistence**: Add PVC support (feature gap)
5. **Platform event correlation**: Add K8s Event watching
6. **mTLS**: Add client TLS secret support
7. **Config cleanup**: Add ImagePullPolicy, make ServiceAccountName configurable
