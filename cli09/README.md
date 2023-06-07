# cli09

## Compatibility

Tested commits in [ckatsak/fc-ctrd](https://github.com/ckatsak/fc-ctrd):

- `cae490b` (branch `uvm-snaps`)
- earlier commits on the `uvm-snaps` branch had different protobuf definitions
  (they did not include `LoadVMSnapshotResponse`)

## Build

### `containerd-client`

To build the `containerd-client` crate, the `protoc` binary needs to be
reachable.
You may either populate `$PROTOC` with the path to the `protoc` binary, or just
make sure it is reachable via `$PATH`.
[Btw, this seems to be a requirement of recent versions of `prost-build`.]

```console
$ PROTOC='/usr/local/protoc/bin/protoc' cargo b
```

### `fc_ctrd/mod.rs`

Create it manually (rather than via `build.rs`), to make sure `*_ttrpc` modules
are also included.

## Run

```console
# rm -rvf /tmp/snapshots/
# RUST_LOG=cli09=trace target/debug/cli09 snap  && echo $?
# RUST_LOG=cli09=trace target/debug/cli09 load  && echo $?
```
