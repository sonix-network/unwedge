# Integrating the vEdge 1000 smoke test into the OpenWrt weekly build

Goal: as the final step of the SONIX-network/openwrt weekly build, boot the
just-built image on real vEdge 1000 hardware and only cut the release if it
boots cleanly — attaching the captured boot log to the release as proof.

## Architecture

```
 GitHub Actions runner (openwrt weekly build)
   │  builds openwrt-…-cisco_vedge1000-initramfs-kernel.bin
   │
   │  uses: sonix-network/unwedge@v1   (this repo's composite action)
   │    └─ unwedge smoke <image>  ──gRPC/TLS over internet──┐
   ▼                                                             ▼
 boot log artifact ◄── boot log ◄──────────────  unwedged (on the controller,
                                                  a vEdge 1000 next to the DUT)
                                                    ├─ serial console (FTDI)
                                                    ├─ APC PDU (SNMP) power
                                                    └─ TFTP server (netboot)
```

The controller runs `unwedged` and is reachable from the internet on its gRPC
port (TLS, ideally mutual TLS). CI never touches the hardware directly; it only
speaks gRPC to `unwedged`, which owns the console, power, and TFTP server.

What the smoke test does, end to end:

1. Uploads the built `initramfs-kernel.bin` into the controller's TFTP store.
2. Starts capturing the serial console.
3. Power-cycles the DUT via the APC PDU (outlet 3) and interrupts U-Boot.
4. `dhcp`, `tftpboot`, optional `crc32 -v`, then `bootoctlinux … coremask=f`.
5. Watches the console for a healthy-boot marker
   (`Please press Enter to activate this console` / BusyBox banner / `procd: - init -`)
   or a failure marker (`Kernel panic`, `not syncing`, …).
6. Writes the full boot log and exits non-zero on failure (gating the release).

## Adding it to the weekly build

In the openwrt weekly workflow, after the image build job, add a job:

```yaml
  smoke-test:
    needs: build            # the job that produced the images
    runs-on: ubuntu-latest
    steps:
      - name: Download built images
        uses: actions/download-artifact@v4
        with:
          name: images       # however the build job named them
          path: bin

      - name: Smoke test on real vEdge 1000
        id: smoke
        uses: sonix-network/unwedge@v1
        with:
          image: bin/**/*-cisco_vedge1000-initramfs-kernel.bin
          boot-log: vedge1000-boot.log
          addr:   ${{ secrets.UNWEDGE_ADDR }}
          ca:     ${{ secrets.UNWEDGE_CA }}
          cert:   ${{ secrets.UNWEDGE_CLIENT_CERT }}
          key:    ${{ secrets.UNWEDGE_CLIENT_KEY }}

      # The action already uploads the boot log as an artifact; to also attach it
      # to the GitHub release:
      - name: Attach boot log to release
        if: always()
        uses: softprops/action-gh-release@v2
        with:
          files: vedge1000-boot.log
          tag_name: ${{ github.ref_name }}
```

Because the `uses:` step exits non-zero on a bad boot, a failing smoke test fails
the job. Make the release-publishing job `needs: smoke-test` so a bad boot blocks
the release.

## Required repository secrets (on the openwrt repo)

| Secret                     | Purpose                                            |
|----------------------------|----------------------------------------------------|
| `UNWEDGE_ADDR`           | `host:port` of the controller's `unwedged`       |
| `UNWEDGE_CA`             | PEM of the CA that signed the server cert          |
| `UNWEDGE_CLIENT_CERT`    | PEM client cert for mutual TLS                     |
| `UNWEDGE_CLIENT_KEY`     | PEM client key for mutual TLS                      |

Use mutual TLS: the controller exposes power and boot control to the internet, so
`unwedged` should be configured with `grpc.tls.client_ca_file` and only issue
client certs to trusted CI.

## Controller setup (once)

1. Cross-compile and install the daemon on the controller vEdge 1000:
   `make daemon-mips64` → copy `bin/unwedged.mips64` to the controller.
2. Wire the FTDI serial adapter to the DUT console and put the DUT on outlet 3.
3. Create `/etc/unwedge/config.yaml` from `config.example.yaml`.
4. Generate TLS certs (see `scripts/gen-certs.sh`) and start `unwedged`
   (a procd init script is provided in `init/unwedged`).

To drive **several devices from one controller**, create one config file per
device (`/etc/unwedge/dut1.yaml`, `dut2.yaml`, …) instead of a single
`config.yaml` — each with its own `grpc.address` port, `serial.device`,
`power.outlet`, and `ssh.host`. Exactly one shares the TFTP server on `:69`
(`tftp.enabled: true`) while the rest set `tftp.enabled: false` and point at the
same `tftp.dir`. The init script starts/stops each instance by its filename stem
(`/etc/init.d/unwedged restart dut1`), and a wildcard/multi-name server cert plus
per-device SRV records let clients reach each by name. See **Several devices on
one controller** in the README for the full walkthrough.

## Notes / current limitations

- The DUT's MAC is not statically settable in hardware, so its DHCP lease/IP may
  vary. The netboot flow uses U-Boot `dhcp` and does not assume a fixed DUT IP.
  A future option-82 static lease per physical port will stabilize SSH access;
  until then prefer console/netboot assertions over SSH in CI.
- Smoke testing uses the **initramfs** image (boots to RAM); it does not touch
  the on-disk installation, so it is safe to run against a persistent DUT.
