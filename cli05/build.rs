// Reports of missing files (like google/protobuf/{empty,any}.proto can be copied from:
//  https://github.com/protocolbuffers/protobuf/tree/main/src/google/protobuf
// or just by including local `protoc`'s installation include/ path.

use ttrpc_codegen::{Codegen, Customize, ProtobufCustomize};

const OUT_DIR: &str = "src/fc_ctrd";

fn main() {
    let protobuf_customized = ProtobufCustomize::default()
        .gen_mod_rs(true)
        //.tokio_bytes(true)
        //.tokio_bytes_for_string(true)
        //.generate_accessors(true)
        .before("/// NOTE(ckatsak): This is auto-generated.");

    Codegen::new()
        .out_dir(OUT_DIR)
        .inputs([
            "proto/events.proto",
            "proto/firecracker.proto",
            "proto/types.proto",
            "proto/service/drivemount/drivemount.proto",
            "proto/service/fccontrol/fccontrol.proto",
            "proto/service/ioproxy/ioproxy.proto",
        ])
        .includes(["proto/"])
        .rust_protobuf()
        .customize(Customize {
            async_all: true,
            ..Default::default()
        })
        .rust_protobuf_customize(protobuf_customized)
        .run()
        .expect("async ttrpc code generation failed")
}
