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

# Release smoke test: upload, netboot, verify healthy boot, save the log.
unwedge -out boot.log smoke openwrt-…-initramfs-kernel.bin
```

The `write` command sends control keys too:
`unwedge write --keys ctrl-x` interrupts U-Boot.

## Using it from an AI agent (MCP)

Run `unwedge-mcp` as an MCP server (stdio). It connects to `unwedged` and
exposes tools: `get_status`, `read_console_log`, `write_console`,
`wait_for_pattern`, `power`, `run_uboot_command`, `interrupt_boot`, `netboot`,
`list_images`, `upload_image`, `delete_image`, `ssh_exec`, and `smoke_test`.

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
  smoke/              release smoke-test engine
  mcp/                minimal MCP stdio server
  config/  tlsutil/   config loading, TLS credentials
cmd/                  unwedged, unwedge, unwedge-mcp
```

## Status / notes

- Developed against the platform docs and the
  [sonix-network/openwrt](https://github.com/sonix-network/openwrt) vEdge 1000 port;
  hardware-in-the-loop tuning of console patterns may still be needed.
- The DUT MAC isn't hardware-settable, so its DHCP IP can vary; the netboot flow
  relies on U-Boot `dhcp` and does not assume a fixed DUT address. A future
  DHCP option-82 static lease per physical port will stabilize SSH.
