# Rootstack operator tools

Manual operator tools extracted from [rsksmart/rootstack](https://github.com/rsksmart/rootstack). The existing monitor/dashboard snapshot is `13be099a53f1e4f9d7d9705943c1e12d5dd2751d`; `rollup-remote` and `rollup-experiment` are from `1a39da1c44576600133d004189cdaa6cac0e5866`.

- `rollup-monitor`: metrics charts, CSV export and traffic simulation.
- [`rollup-dashboard`](cmd/rollup-dashboard/README.md): log streaming, health and component controls.
- [`rollup-remote`](cmd/rollup-remote/ARCHITECTURE.md): SSH control panel for already-running remote services.
- `rollup-experiment`: local experiment runner with traffic, batcher sweeps and reports.

## Build and check

Use the Go version in `go.mod`. This repository builds the four operator tools without a sibling checkout. The RSK optimism fork remains pinned to the rootstack gitlink commit in `go.mod`.

```sh
go build ./...
go test ./...
go vet ./...
```

```sh
go build -o bin/rollup-monitor ./cmd/rollup-monitor
go build -o bin/rollup-dashboard ./cmd/rollup-dashboard
go build -o bin/rollup-remote ./cmd/rollup-remote
go build -o bin/rollup-experiment ./cmd/rollup-experiment
```

Use `--help` for flags. Existing monitor/dashboard examples:

```sh
./bin/rollup-monitor --l1-rpc=http://localhost:4444 --l2-rpc=http://localhost:8545 --node-rpc=http://localhost:9545 --workdir=/path/to/deployer-output
./bin/rollup-dashboard --attach --l1-rpc=http://localhost:4444 --l2-rpc=http://localhost:8545 --node-rpc=http://localhost:9545
```

The monitor creates `rollup-monitor.toml` if it does not exist. Start an experiment from the included sample:

```sh
cp experiment.example.toml my-experiment.toml
./bin/rollup-experiment --config my-experiment.toml
```

These tools can send transactions or control services; use test accounts and verify endpoints before enabling those operations.

## Extraction boundary and ownership

This repository owns the four manual operator commands above, the monitor library under `cmd/rollup-monitor/monitor`, its contract assets, and the supporting `rsk/` and `gorsk/` packages needed to build independently. `rollup-experiment` uses that existing monitor library; it does not duplicate rootstack's `rsk/monitor` package.

Rootstack remains responsible for current deployer/runtime and verification commands, including `./cmd/oprsk-batcher`, other current `oprsk-*` binaries, `rollup-healthcheck`, and `cmd/verify-regtest`. Build `oprsk-batcher` in a rootstack checkout, then pass its path to `rollup-experiment --batcher-bin` (or `target.batcher_bin`).

The experiment's `--rollup-node-bin`/`--rollup-node-config` support is preserved for legacy local setups that provide those files; this repository does not build that binary, and it is not current multi-host support. `rollup-remote` connects to and controls components built and deployed by rootstack; it does not build or deploy them.

Existing source attribution, behavior, assets and tests are preserved. These manual tools do not replace the `just verify-regtest` gate.
