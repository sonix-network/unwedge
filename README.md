# unwedge

Remote control plane for doing **kernel and OS development on a Cisco vEdge 1000**
(Octeon CN6130, MIPS64 big-endian) from an AI agent or CI.

It turns a controller box — itself a vEdge 1000 running OpenWrt — into a service
that owns everything you need to iterate on the device-under-test (DUT):

- **Serial console** over an FTDI USB-UART (115200 8N1): stream, scrollback, write, regex-wait.
- **Power** via an APC switched rack PDU over SNMP (the DUT is on outlet 3): on/off/cycle.
- **U-Boot orchestration**: interrupt the autoboot (`Ctrl-X`), run commands, and the full
  **netboot** recipe (`dhcp` → `tftpboot` → `bootoctlinux … coremask=f`).
- **TFTP image store**: upload kernels and serve them to the DUT's U-Boot.
- **SSH** into the booted target.
- **Release smoke test**: netboot a freshly built image and prove a clean boot with a captured log.

Everything is exposed as a **gRPC API over TLS** (mutual TLS supported) so it can
be driven safely over the internet.

## Components

| Binary        | Runs on                   | Purpose                                                   |
|---------------|---------------------------|-----------------------------------------------------------|
| `unwedged`    | controller (linux/mips64) | gRPC daemon owning serial, power, TFTP, U-Boot, SSH       |
| `unwedge`     | anywhere (CI, laptop)     | CLI client; includes the `smoke` release test            |
| `unwedge-mcp` | the agent's machine       | MCP (Model Context Protocol) server bridging tools → gRPC |

```
  AI agent   ──▶ unwedge-mcp ──┐
                               │  gRPC / TLS
  CI / human ──▶ unwedge CLI ──┴──▶ unwedged  (the controller, a vEdge 1000)
                                       │
                                       │ owns the target and drives it via:
                                       ├─ serial console   (FTDI USB-UART @ 115200)
                                       ├─ power            (APC PDU over SNMP, outlet 3)
                                       ├─ U-Boot + TFTP    (interrupt & netboot kernels)
                                       └─ SSH              (shell on the booted target)
                                       │
                                       ▼
                                 vEdge 1000  (DUT)
```

## Build

```sh
make build          # host binaries into ./bin
make daemon-mips64  # unwedged for the vEdge 1000 controller (big-endian MIPS64)
make release        # full cross-compile matrix
make test race vet  # tests (with -race) and static checks
```

Requires Go 1.24+. Regenerating the gRPC stubs (`make proto`) additionally needs
`buf`, `protoc-gen-go`, and `protoc-gen-go-grpc` on `PATH`; the generated code in
`gen/` is checked in, so normal builds don't need them.

## Releasing

Releases are cut by pushing a semver tag; GitHub Actions builds and publishes the
artifacts. On a `vMAJOR.MINOR.PATCH` tag the `Release` workflow runs the tests,
builds the deterministic per-arch tarballs via `make dist` — **mips64** (the
big-endian vEdge 1000 controller), **amd64**, and **arm64** — and creates a
GitHub Release with `unwedge-*-linux-*.tar.gz` and `SHA256SUMS` attached.

```sh
git tag v0.2.2
git push origin v0.2.2          # → builds and publishes the release
```

It can also be run by hand without pushing a tag:

```sh
gh workflow run Release -f version=v0.2.2   # or Actions → Release → "Run workflow"
```

After the release is published, point the OpenWrt feed at it so the image build
pulls the new binaries: in
[sonix-network/openwrt-packages](https://github.com/sonix-network/openwrt-packages),
edit `utils/unwedge/Makefile` — bump `PKG_VERSION`, reset `PKG_RELEASE:=1`, and
update the three `PKG_HASH_*` from the release's `SHA256SUMS` — then open a PR.

## Configure & run the daemon

```sh
cp config.example.yaml /etc/unwedge/config.yaml   # then edit
scripts/gen-certs.sh <controller-hostname-or-ip>    # TLS (mutual) material
unwedged -config /etc/unwedge/config.yaml
```

On the OpenWrt controller, install `init/unwedged` as `/etc/init.d/unwedged`
and `enable` it. See `config.example.yaml` for every option; defaults match stock
vEdge 1000 U-Boot (prompt `=>`, interrupt on `Hit ctrl-x to stop booting`,
`octmgmt0`, `loadaddr 0x20000000`, `coremask=f`).

## CLI examples

The client reads its daemon address and TLS material from three layers, in
precedence order **flag > environment > config file > built-in default**, so you
can prime the common values once and keep just the subcommand on the command
line. Point at a config file with `-config` or `UNWEDGE_CONFIG`; otherwise
`~/.config/unwedge/config.yaml` is loaded when present (`$XDG_CONFIG_HOME` is
honored). Paths may use `~`. The daemon port defaults to `7777`, so `addr` can
omit it.

```yaml
# ~/.config/unwedge/config.yaml
addr: unwedge-oob-lab-sw1.sonix.network   # :7777 is implied
ca:   ~/unwedge/ca.crt
cert: ~/unwedge/unwedge-bastion.crt
key:  ~/unwedge/unwedge-bastion.key
# server_name: ""   # override TLS server name
# no_tls: false     # connect without TLS (local/testing)
# insecure: false   # skip server cert verification (dev only)
```

With that primed, the long invocation collapses to just `unwedge status`. The
equivalent environment overrides (which win over the file) are:

```sh
export UNWEDGE_ADDR=controller.example:7777
export UNWEDGE_CA=certs/ca.crt UNWEDGE_CERT=certs/client.crt UNWEDGE_KEY=certs/client.key

unwedge status
unwedge console                       # live serial (Ctrl-C to stop)
unwedge power cycle
unwedge image upload openwrt-…-initramfs-kernel.bin
unwedge netboot --verify openwrt-…-initramfs-kernel.bin
unwedge uboot 'printenv'
unwedge ssh 'uname -a'

# Copy files to/from the target. Prefix the target-side path with ':'.
unwedge scp ./initrd.cpio :/tmp/initrd.cpio    # upload
unwedge scp :/proc/config.gz ./config.gz       # download

# Release smoke test: upload, netboot, verify healthy boot, save the log.
unwedge -out boot.log smoke openwrt-…-initramfs-kernel.bin
```

The `write` command sends control keys too:
`unwedge write --keys ctrl-x` interrupts U-Boot.

### Reaching the target through the daemon (ProxyCommand)

The daemon can reach the target even when your workstation cannot. `unwedge ssh
-W` turns the CLI into a raw SSH proxy, so your local `ssh`/`scp`/`rsync` can
tunnel to the target through the daemon (SSH auth stays end-to-end — the daemon
only shuffles bytes):

```sh
ssh -o ProxyCommand="unwedge ssh -W" root@target
scp -o ProxyCommand="unwedge ssh -W" file root@target:/tmp/
```

The built-in `unwedge scp` (above) is the credential-free alternative: it copies
over the daemon's own SSH connection using the classic scp protocol (remote `scp
-t`/`scp -f`), so it needs no local keys and no SFTP subsystem on the target.

`unwedge ssh` and `unwedge scp` accept `--host host[:port]` to target a
different host on the target's network, and `-W` accepts an optional `host:port`
(e.g. OpenSSH's `%h:%p`) for the same purpose.

## Session locking (multiple agents, one unit)

The hardware is single-user. When session locking is enabled (default), a client
must hold an exclusive **session** to run any operational RPC. Read-only
observation is lock-free — `GetStatus` (so the lock is always visible) and the
console readers (`StreamConsole`, `ReadConsoleLog`) so anyone can watch what the
holder is doing without locking them out. `StartSession` blocks until the lock is
free, every call refreshes the holder's TTL, and an idle session auto-releases
after `session.ttl` (default 5m) so a crashed client can't hold the unit forever.
The session ID rides in gRPC metadata (`unwedge-session-id`) via client/server
interceptors — callers don't plumb it through.

This is handled for you:
- **CLI**: each command transparently acquires the lock (waiting if held, with a
  notice), keeps it alive while running, and releases on exit. `unwedge status`
  shows who holds it. Flags: `--session-wait`, `--session-owner`, `--no-session`.
- **MCP**: the bridge lazily acquires on the first operational tool, refreshes on
  each call, and releases after idle (or via the `release_lock` tool). Extra
  tools: `acquire_lock`, `release_lock`; `get_status` reports the lock.

A client talking to a daemon without locking (older build) degrades gracefully.

## Using it from an AI agent (MCP)

Run `unwedge-mcp` as an MCP server (stdio). It connects to `unwedged` and
exposes tools: `get_status`, `acquire_lock`, `release_lock`, `read_console_log`,
`write_console`, `wait_for_pattern`, `power`, `run_uboot_command`,
`interrupt_boot`, `netboot`, `list_images`, `upload_image`, `delete_image`,
`ssh_exec`, and `smoke_test`.

Example Claude Code / MCP client config:

```json
{
  "mcpServers": {
    "unwedge": {
      "command": "unwedge-mcp",
      "args": ["-addr", "controller.example:7777"],
      "env": {
        "UNWEDGE_CA":   "/path/certs/ca.crt",
        "UNWEDGE_CERT": "/path/certs/client.crt",
        "UNWEDGE_KEY":  "/path/certs/client.key"
      }
    }
  }
}
```

`unwedge-mcp` shares the CLI's config resolution, so if `~/.config/unwedge/config.yaml`
(or `UNWEDGE_CONFIG`) is already primed you can drop the `args` and `env` here.

## CI: gate the OpenWrt weekly release on a real boot

This repo ships a composite GitHub Action (`action.yml`). Add it as the final
step of the SONIX-network/openwrt weekly build to boot the just-built image on
real hardware and attach the boot log to the release. See
[docs/openwrt-integration.md](docs/openwrt-integration.md).

## Layout

```
proto/                gRPC API definition (source of truth)
gen/                  generated Go stubs (checked in)
internal/
  serialconsole/      console ring buffer, fan-out, regex wait, control keys
  serialport/         opens the physical FTDI device
  power/              APC PDU SNMP controller (+ fake)
  uboot/              interrupt/netboot orchestration
  tftp/               image store + read-only TFTP server
  sshexec/            SSH command execution
  server/             gRPC service wiring
  client/             shared gRPC client (CLI + MCP)
  clientconfig/       client-side defaults (addr/ca/cert/key) file + env
  smoke/              release smoke-test engine
  mcp/                minimal MCP stdio server
  config/  tlsutil/   daemon config loading, TLS credentials
cmd/                  unwedged, unwedge, unwedge-mcp
```

## Status / notes

- Developed against the platform docs and the
  [sonix-network/openwrt](https://github.com/sonix-network/openwrt) vEdge 1000 port;
  hardware-in-the-loop tuning of console patterns may still be needed.
- The DUT MAC isn't hardware-settable, so its DHCP IP can vary; the netboot flow
  relies on U-Boot `dhcp` and does not assume a fixed DUT address. A future
  DHCP option-82 static lease per physical port will stabilize SSH.
