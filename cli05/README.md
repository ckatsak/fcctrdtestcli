# cli05

## Build

### `containerd-client`

To build the `containerd-client` crate, the `protoc` binary needs to be
reachable.
You may either populate `$PROTOC` with the path to the `protoc` binary, or just
make sure it is reachable via `$PATH`.
[Btw, this seems to be a requirement of recent versions of `prost-build`.]

### `fc_ctrd/mod.rs`

Create it manually (rather than via `build.rs`), to make sure `*_ttrpc` modules
are also included.
