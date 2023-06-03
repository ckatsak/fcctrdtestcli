# cli08

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
# RUST_LOG=cli08=trace target/debug/cli08 snap  && echo $?
# RUST_LOG=cli08=trace target/debug/cli08 load  && echo $?
```
