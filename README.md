# mpts2spts

Splits multicast MPTS inputs into single-program SPTS outputs (UDP or RTP), managed from a web UI. It ships as one Go binary with the UI embedded and keeps its configuration in SQLite.

## Build & run

```sh
go build -ldflags="-s -w" -o mpts2spts .
./mpts2spts -listen :8080 -db mpts2spts.db -env .env
```

`.env` holds the web login:

```
USERNAME=admin
PASSWORD=change-me
```

Flags: `-listen` (web address), `-db` (SQLite file), `-env` (credentials file), `-pprof` (debug profiler address, off by default).

For multicast on several interfaces, run as root or give the binary `CAP_NET_RAW` so `SO_BINDTODEVICE` works: `sudo setcap cap_net_raw+ep mpts2spts`. Without it, interface selection still works through `IP_MULTICAST_ALL=0` and the per-interface join.

Systemd: see `mpts2spts.service`.

## Usage

- **Inputs**: there are four ways to add them.
  - **Discover all** (default): pick an interface, or all interfaces, and the app listens for a few seconds. It finds every TS stream on any IP and any port: unicast, multicast, UDP or RTP. Each result shows the sender, bitrate and programs, and you add them one by one or with **Add all**. This needs `CAP_NET_RAW` (`sudo setcap cap_net_raw+ep mpts2spts`, or run as root; the systemd unit already grants it). It can only see multicast the switch actually delivers; behind IGMP snooping, use **Scan range**.
  - **By interface**: pick a local address once, then enter any number of groups, one per line or comma separated. Ranges are accepted (`239.2.2.1-239.2.2.20` or `239.2.2.1-20`), and `:port` on an entry overrides the default port. All of them are added in one step, and groups that already exist are skipped.
  - **Manual URL**: type a full URL.
  - **Scan**: joins a range of groups on one interface and lists the ones that carry TS. Add them one by one, or all at once with **Add all**.

  URL form:
  `udp://239.2.2.2:1234?localaddr=192.168.10.10` (optionally `&sources=<ip>` for SSM). `rtp://` inputs are accepted too, and an RTP header is detected and stripped automatically.
- **Programs**: each running input lists its services (name and provider from the SDT, plus streams and bitrate). Tick several and use **Create outputs**, or use **+ Output** on one row.
- **Outputs**: UDP or RTP, with `localaddr` choosing the egress interface. Leave the address empty to allocate the next free `ip:port` from the pool in **Settings**, or type one. Service ID and TS ID are optional overrides.
- **Playlist**: downloads an M3U of every enabled output.
- **Start all / Stop all** (top bar) enable or disable every input. This is saved, so it persists across restarts. **Restart all** reopens every socket and leaves the configuration unchanged.
- On each outputs table, the **All** toggle enables or disables every output shown, and **Delete all** removes every output shown. Both act on the current search results, so you can search first and then act on a subset.
- **Search**: the box in the top bar searches inputs, programs and outputs together. Each table also has its own search box, and the sidebar has an input filter.
- The top bar shows host CPU, host RAM and the app's own CPU (% of one core) and memory.

## Stable output across restarts

Each output's identity is stored in SQLite when you create it: service ID, TS ID, ONID, program name, provider, service type and destination. On every start the app rebuilds PAT and SDT from these stored values. Version numbers are derived from the content, so the PSI comes out byte-identical after a restart. The PMT is the source PMT with only `program_number` changed, and ES PIDs pass through unchanged. If the source later renames a service, the output keeps its stored name until you edit it.

## Performance

There is one OS thread per input. It joins the MPTS once and reads with blocking `recvmmsg` (64 datagrams per call), paced by 2 ms when the socket buffer is shallow. Each TS packet costs one PID-table lookup plus a 188-byte copy for each output that wants it. Output goes out through `sendmmsg`, batched per interface. There are no per-packet allocations.

Measured on the test stream (126 Mbps MPTS with 12 programs and 3 outputs): about 9.5% of one core and 11 MB RSS.
