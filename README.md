# Rootstack operator tools

Manual operator TUIs extracted from [rsksmart/rootstack](https://github.com/rsksmart/rootstack) at commit `13be099a53f1e4f9d7d9705943c1e12d5dd2751d`.

- `rollup-monitor`: metrics charts, CSV export and traffic simulation.
- [`rollup-dashboard`](cmd/rollup-dashboard/README.md): log streaming, health and component controls.

## Build and check

Use the Go version in `go.mod`. No sibling checkout or submodule is required. The RSK optimism fork is pinned in `go.mod` to the source repository's gitlink commit.

```sh
go build -o bin/rollup-monitor ./cmd/rollup-monitor
go build -o bin/rollup-dashboard ./cmd/rollup-dashboard
go test ./...
go vet ./...
```

```sh
./bin/rollup-monitor --l1-rpc=http://localhost:4444 --l2-rpc=http://localhost:8545 --node-rpc=http://localhost:9545 --workdir=/path/to/deployer-output
./bin/rollup-dashboard --attach --l1-rpc=http://localhost:4444 --l2-rpc=http://localhost:8545 --node-rpc=http://localhost:9545
```

Use `--help` for the current flags. Both tools retain their original configuration formats. The monitor creates `rollup-monitor.toml` if it does not exist. These tools can send transactions or control services; use test accounts and verify endpoints before enabling those operations.

## Extraction boundary

The complete command and package sources are copied here, including monitor contract assets. Supporting `rsk/` and `gorsk/` packages are the transitive local dependencies needed to compile independently; their existing tests are retained. Imports now use this module path. Existing source attribution is preserved.

`rollup-healthcheck`, `rollup-experiment`, and `cmd/verify-regtest` stay in rootstack. Rootstack retains the non-TUI monitor library for its healthcheck and experiment callers under `rsk/monitor`. These manual tools do not replace the `just verify-regtest` gate.
