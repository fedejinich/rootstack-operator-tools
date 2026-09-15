# Rollup Dashboard

Interactive TUI for a node operator: live log viewing, health monitoring, and
component controls (pause/resume sequencer/batcher/proposer via admin RPC).

There are two modes:

- **Attach (recommended)** — `--attach` monitors and controls an
  already-running node (e.g. the per-service compose/systemd stack) over its
  RPC + admin-RPC + Prometheus endpoints. It does **not** spawn a subprocess.
- **Supervised** — without `--attach`, the dashboard starts and manages a node
  binary (`--rollup-node-bin`) as a child process and streams its logs. This
  mode predates the per-service split and has not kept up with it: it spawns the
  binary with `--config`, which `oprsk-node` does not accept, and there is no
  single `rollup-node` binary anymore. Use `--attach`. PAYROLLUP-146 decides
  whether supervised mode is fixed or removed.

## Overview

The dashboard:

- **Streams logs** with filtering by level (TRACE..CRIT) and component (EXECUTION, CONSENSUS, BATCHER, PROPOSER) — supervised mode only
- **Monitors health** of all layers (L1, L2, op-node, batcher, proposer) via RPC and Prometheus
- **Controls components** in real-time: pause/resume sequencer, batcher, and proposer via admin RPC
- **Shows batcher settings** read-only; they are `oprsk-batcher` flags now, so the pane can no longer apply them
- **Manages accounts** view balances and fund batcher/proposer accounts

## Prerequisites

For **attach** mode: a running node and reachable RPC / op-node / metrics
endpoints (point at them with the flags below). For **supervised** mode: a node
binary (`--rollup-node-bin`), a TOML config, and the deploy artifacts it
references (`rollup.json`, `genesis.json`, …).

## Quick Start

```bash
# Attach to an already-running (possibly remote) node — no subprocess:
go run ./cmd/rollup-dashboard/... --attach \
  --l2-rpc http://127.0.0.1:8545 \
  --node-rpc http://127.0.0.1:9545 \
  --batcher-rpc http://127.0.0.1:8548 \
  --node-metrics http://127.0.0.1:7300/metrics \
  --batcher-metrics http://127.0.0.1:7301/metrics

# Supervised: start and manage a node binary from a TOML config
go run ./cmd/rollup-dashboard/... --rollup-node-bin ./oprsk-node --config ./rollup-node.toml

# Auto-discover: if rollup-node.toml exists in the current directory it is
# picked up automatically (supervised mode)
go run ./cmd/rollup-dashboard/...
```

## Command-Line Flags

| Flag | Env Var | Default | Description |
| --- | --- | --- | --- |
| `--attach`, `-a` | `ROLLUP_DASHBOARD_ATTACH` | `false` | Attach to a running node instead of spawning one |
| `--config`, `-c` | `ROLLUP_NODE_CONFIG` | auto-discover | Path to rollup-node TOML config file |
| `--rollup-node-bin` | `ROLLUP_NODE_BIN` | `rollup-node` | Node binary to supervise (non-attach mode) |
| `--l1-rpc` | `L1_RPC_URL` | from TOML | L1 RPC URL — **attach mode only** |
| `--l2-rpc` | `L2_RPC_URL` | `:8545` | L2 execution RPC URL — **attach mode only** |
| `--node-rpc` | `NODE_RPC_URL` | `:9545` | op-node RPC URL — **attach mode only** |
| `--batcher-rpc` | `BATCHER_RPC_URL` | `:8548` | op-batcher admin RPC URL — **attach mode only** |
| `--node-metrics` / `--batcher-metrics` / `--proposer-metrics` | - | built-in ports | Prometheus metrics URLs — **attach mode only** (needed since a remote node's local TOML won't enable them here) |

> The endpoint/metrics flags apply in `--attach` mode only. The env names are suffixed (`*_RPC_URL`) on purpose so the dev stack's pervasive `L1_RPC`/`L2_RPC` (which point at container-internal hosts) can't silently override them.
>
> Only `l1_rpc` and `master_private_key` still come from `rollup-node.toml`. The endpoints and metrics URLs used to be derived from its `[batcher]`, `[node]` and `[proposer]` sections, which the shipped templates no longer carry, so those now fall back to the built-in local ports above. Pass the flags when your ports differ.
| `--log-buffer` | - | `10000` | Log ring buffer capacity (number of lines kept in memory) |

## TUI Layout

```text
┌──────────────────────────────────────────────────────┐
│ Rollup Dashboard — node: running (pid 12345)         │
│ L1: #1234  L2: #5678 (unsafe:5678 safe:5670 final:0) │
│ ● SEQ  ● BATCH  ● PROP  ● DERIV │ node:UP  L1lag:0s │
│ pending: 5 blks / 1234 B  │  proposer: #10          │
├──────────────────────────────────────────────────────┤
│ LOG PANE (expandable, ~60% height)                   │
│ [level filter: 1-TRACE 2-DEBUG 3-INFO ...]           │
│ [context filter: e-EXEC c-CONS b-BATCH p-PROP]      │
│                                                      │
├──────────────────────────────────────────────────────┤
│ BATCHER SETTINGS (collapsible, Tab to toggle)        │
│ ▸ Max L1 Tx Size:  120000                            │
│   Compression Algo: zlib                             │
├──────────────────────────────────────────────────────┤
│ [?] help [$] accounts [S/B/P] toggle [F] flush [q]   │
└──────────────────────────────────────────────────────┘
```

### LED Indicators

The health bar shows car-dashboard-style LED indicators for each component:

| LED | State | Meaning |
| --- | --- | --- |
| `●` green | Active | Component is running normally |
| `◑` yellow | Pending | Command in progress (starting/stopping) |
| `◐` yellow | Paused | Component is paused (controllable via S/B/P) |
| `○` red/gray | Down/Idle | Component is down or idle |

Components:

- **SEQ** — Sequencer (L2 block production)
- **BATCH** — Batcher (posting batches to L1)
- **PROP** — Proposer (posting output roots to L1)
- **DERIV** — Derivation pipeline (re-deriving L2 from L1)

## Keyboard Shortcuts

### General

| Key | Action |
| --- | --- |
| `?` | Toggle help overlay |
| `$` | Toggle accounts panel (view/fund batcher/proposer) |
| `Tab` | Switch focus between Log and Settings panes |
| `l` | Expand/collapse log to full screen |
| `q` / `Ctrl+C` | Quit (sends SIGINT to rollup-node) |

### Node Controls

Control individual components without restarting the entire node:

| Key | Action |
| --- | --- |
| `S` | Toggle sequencer — pause/resume L2 block production |
| `B` | Toggle batcher — pause/resume batch posting to L1 |
| `P` | Toggle proposer — pause/resume output root proposals |
| `F` | Flush batcher — force immediate posting of pending data |

These use the admin RPC endpoints (`admin_startSequencer`, `admin_stopBatcher`, etc.) and work in real-time without restarting the node.

**Use case**: If the batcher has a large backlog of pending blocks, press `S` to pause the sequencer (stop producing new L2 blocks), let the batcher catch up, then press `S` again to resume.

### Log Pane

| Key | Action |
| --- | --- |
| `1`-`6` | Toggle log level filter (1=TRACE, 2=DEBUG, 3=INFO, 4=WARN, 5=ERROR, 6=CRIT) |
| `e` | Toggle EXECUTION logs |
| `c` | Toggle CONSENSUS logs |
| `b` | Toggle BATCHER logs |
| `p` | Toggle PROPOSER logs |
| `g` | Toggle GENERAL (dashboard) logs |
| `j` / `↓` | Scroll down |
| `k` / `↑` | Scroll up |
| `PgDn` / `PgUp` | Page scroll |
| `G` / `End` | Jump to bottom (latest) |
| `Home` | Jump to top (oldest) |

### Settings Pane

| Key | Action |
| --- | --- |
| `↑` / `↓` | Select field |
| `Enter` | Edit selected field |
| `Space` | Cycle option (for enum fields like compressor, compression_algo) |
| `Esc` | Cancel edit |
| `A` | Reports that these are `oprsk-batcher` flags now (see below) |
| `Tab` | Collapse settings and return to log pane |

## Batcher Settings

The pane shows the current values. **It can no longer apply them**, so treat it
as a viewer:

| Field | Description | Default |
| --- | --- | --- |
| `max_l1_tx_size` | Max batch tx size in bytes | `120000` |
| `max_channel_duration` | Max L1 blocks before sealing (0 = unlimited) | `0` |
| `sub_safety_margin` | L1 blocks subtracted from channel timeout | `10` |
| `max_pending_transactions` | Max in-flight L1 txs | `1` |
| `poll_interval` | How often to poll L2 for new blocks | `2s` |
| `compressor` | Compressor type: shadow, ratio, none | `shadow` |
| `compression_algo` | Algorithm: zlib, brotli, brotli-9, etc. | `zlib` |
| `target_num_frames` | Target frames per channel | `1` |
| `approx_compr_ratio` | Approximate compression ratio (for ratio compressor) | `0.4` |
| `batch_type` | 0 = SingularBatch, 1 = SpanBatch | `0` |
| `data_availability_type` | calldata, blobs, or auto | `calldata` |

`A` used to write a `[batcher]` section into `rollup-node.toml` and restart the
node. Both halves stopped working: `oprsk-batcher` takes these as CLI flags and
does not read the file, and the deploy tooling now rejects a `[batcher]` section
outright, so the write left `rollup-node.toml` unloadable for `just deploy`.

Set them on `oprsk-batcher` instead. `compose.yml` passes the ones worth tuning
and `.env` overrides them; `docs/config-tuning.md` has the per-environment
values. PAYROLLUP-146 decides whether this pane stays, moves to the admin RPC,
or goes.

## Health Bar Glossary

The health bar at the top of the TUI shows live rollup state. Press `?` in the dashboard for the built-in glossary.

### Block heads

| Label | Meaning |
| --- | --- |
| L1 | Latest L1 (RSK) block seen by the node |
| L2 / unsafe | L2 chain tip -- the latest block the sequencer has produced |
| safe | Highest L2 block fully derived from L1 batch data |
| finalized | Highest L2 block derived from finalized L1 data |
| lag | `unsafe - safe`: blocks sequenced but not yet confirmed via L1 |

The invariant is always `finalized <= safe <= unsafe`.

### Block lifecycle

```text
Sequencer produces block  ──►  unsafe head (chain tip)
Batcher posts batch to L1 ──►  derivation re-derives it  ──►  safe head
L1 block becomes finalized ──►  finalized head
```

### Process & pipeline

| Label | Meaning |
| --- | --- |
| SEQ / BATCH / PROP / DERIV | LED indicators showing component state (see LED Indicators above) |
| node | Process liveness (UP/DOWN) |
| L1lag | Seconds between the latest L1 block timestamp and wall-clock time |

### Batcher metrics

| Label | Meaning |
| --- | --- |
| pending | Blocks the batcher has queued but not yet posted to L1 |
| bumps | Gas-price bumps the batcher performed on stuck L1 transactions |
| bal | Batcher's L1 account balance (funds the L1 tx fees) |

### Proposer metrics

| Label | Meaning |
| --- | --- |
| proposed | Latest L2 output root sequence number submitted on-chain |
| bal | Proposer's L1 account balance |

## Health Monitoring

The health bar polls the following endpoints. They come from the built-in local ports below unless you pass the `--*-rpc` / `--*-metrics` flags, which is how you point attach mode at a running node:

| Source | Data |
| --- | --- |
| L2 RPC (`:8545`) | Latest L2 block number |
| L1 RPC | Latest L1 block number |
| op-node RPC (`:9545`) | Sync status (unsafe/safe/finalized L2 heads) |
| Node Prometheus | Process liveness, derivation state, L1 latency |
| Batcher Prometheus | Pending blocks/bytes, gas bumps, balance |
| Proposer Prometheus | Proposed sequence number, balance |

## See Also

- [rollup-monitor](../rollup-monitor/) -- Standalone monitoring TUI (read-only; attaches to a running node; shares the Prometheus-scrape and sync-status code this dashboard now imports)
- [rollup-healthcheck](https://github.com/rsksmart/rootstack/tree/main/cmd/rollup-healthcheck) -- One-shot / `--watch` health checker for a running rollup
- `oprsk-node` ([rootstack cmd/oprsk-node](https://github.com/rsksmart/rootstack/tree/main/cmd/oprsk-node)) -- the consensus node binary; in the per-service stack the dashboard attaches to it rather than spawning it
