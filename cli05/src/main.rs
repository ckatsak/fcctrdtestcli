mod fc_ctrd;

use std::{io, path::PathBuf};

use anyhow::{anyhow, Context, Result};
use argh::FromArgs;
use const_format::concatcp;
use containerd_client::{
    services::v1::{
        containers_client::ContainersClient, images_client::ImagesClient,
        tasks_client::TasksClient, version_client::VersionClient, GetImageRequest, Image,
    },
    with_namespace,
};
use tokio::time::Instant;
use tonic::{transport::Channel, Request};
use tracing::{debug, info, instrument, Level};
use tracing_subscriber::{fmt::format::FmtSpan, EnvFilter};
use ttrpc::{
    asynchronous::Client as TtrpcClient,
    context::{self, Context as TtrpcContext},
};

use fc_ctrd::fccontrol_ttrpc::FirecrackerClient;
use fc_ctrd::firecracker::{CreateVMRequest, StopVMRequest};

/// Command-line client to create or load uVM snapshots.
#[derive(FromArgs, Debug)]
struct Cli {
    #[argh(subcommand)]
    command: Command,
}

#[derive(FromArgs, Debug)]
#[argh(subcommand)]
enum Command {
    CreateSnapshot(SnapArgs),
    LoadSnapshot(LoadArgs),
}

/// TODO(ckatsak): Document subcommand 'snap'
#[derive(FromArgs, Debug)]
#[argh(subcommand, name = "snap")]
struct SnapArgs {
    /// image URL; e.g., "docker.io/library/nginx:latest"
    #[argh(option, short = 'i', default = "conf::NGINX_IMAGE_NAME.into()")]
    image: String,
}

/// TODO(ckatsak): Document subcommand 'load'
#[derive(FromArgs, Debug)]
#[argh(subcommand, name = "load")]
struct LoadArgs {
    /// snapshot directory path; e.g., "/tmp/snapshots"
    #[argh(option, short = 'p', default = "PathBuf::from(conf::SNAPSHOT_DIRNAME)")]
    dir_path: PathBuf,
}

//#[derive(Debug)]
struct Client {
    c_channel: Channel,
    vc: VersionClient<Channel>,
    cc: ContainersClient<Channel>,
    ic: ImagesClient<Channel>,
    tc: TasksClient<Channel>,

    fcc: FirecrackerClient,
}

impl Client {
    #[instrument(level = Level::TRACE)]
    pub async fn new() -> Result<Self> {
        info!("Creating new containerd client...");
        let c_channel = containerd_client::connect(conf::CONTAINERD_ADDRESS)
            .await
            .with_context(|| {
                format!(
                    "failed to connect to containerd through '{}'",
                    conf::CONTAINERD_ADDRESS
                )
            })?;
        let vc = VersionClient::new(c_channel.clone());
        let cc = ContainersClient::new(c_channel.clone());
        let ic = ImagesClient::new(c_channel.clone());
        let tc = TasksClient::new(c_channel.clone());

        info!("Creating new fc-control client...");
        let fcc = FirecrackerClient::new(
            TtrpcClient::connect(concatcp!("unix://", conf::CONTAINERD_TTRPC_ADDRESS))
                .with_context(|| {
                    format!(
                        "failed to connect to fc-control plugin through '{}'",
                        conf::CONTAINERD_TTRPC_ADDRESS
                    )
                })?,
        );

        Ok(Self {
            c_channel,
            vc,
            cc,
            ic,
            tc,

            fcc,
        })
    }

    #[instrument(level = Level::TRACE, skip(self))]
    pub async fn get_image(&mut self, image_name: &str) -> Result<Image> {
        let req = GetImageRequest {
            name: image_name.into(),
        };
        self.ic
            .get(with_namespace!(req, conf::NAMESPACE))
            .await
            .with_context(|| format!("failed to get image '{image_name}'"))?
            .into_inner()
            .image
            .ok_or_else(|| anyhow!("containerd returned an empty response"))
    }

    /// Retrieve version information for the containerd we have connected to.
    pub async fn version(&mut self) -> Result<(String, String)> {
        let resp = self
            .vc
            .version(())
            .await
            .with_context(|| "failed to retrieve version information")?
            .into_inner();
        Ok((resp.version, resp.revision))
    }
}

#[instrument(level = Level::TRACE)]
async fn create_snapshot(args: SnapArgs) -> Result<()> {
    let mut c = Client::new()
        .await
        .with_context(|| "failed to create new Client")?;

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Version info for containerd
    let (version, revision) = c.version().await?;
    info!("containerd {{ version: {version}, revision: {revision} }})");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Fetch the container image to be deployed inside the uVM
    info!("Fetching the container image...");
    let image = c
        .get_image(&args.image)
        .await
        .with_context(|| "failed to get container image")?;
    info!("Found: {image:?}");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    let t_start = Instant::now();

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Create TTRPC context to use across all calls
    //let ctx = TtrpcContext {
    //    metadata: [(
    //        conf::TTRPC_HEADER_NAMESPACE_KEY.into(),
    //        vec![conf::NAMESPACE.into()],
    //    )]
    //    .into(),
    //    timeout_nano: conf::CLIENT_SIDE_TIMEOUT_NS, // 6 sec
    //};
    let mut ctx = context::with_timeout(conf::CLIENT_SIDE_TIMEOUT_NS); // 6 sec
    ctx.add(
        conf::TTRPC_HEADER_NAMESPACE_KEY.to_string(),
        conf::NAMESPACE.into(),
    );
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Create new uVM
    info!("Creating new uVM...");
    let create_req = CreateVMRequest {
        VMID: conf::VMID.into(),
        KernelArgs: conf::KERNEL_ARGS.into(),
        NetworkInterfaces: vec![conf::NETWORK_INTERFACE.clone()],
        ContainerCount: 1,
        ExitAfterAllTasksDeleted: true,
        TimeoutSeconds: conf::SERVER_SIDE_TIMEOUT_SEC,
        ..Default::default()
    };
    let create_resp = c
        .fcc
        .create_vm(ctx.clone(), &create_req)
        .await
        .with_context(|| "failed to create new uVM")?;
    debug!("fc-control responded: {create_resp:?}");
    // TODO(ckatsak): Cleanup the new uVM on exit
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Create the new container to run inside the uVM
    //info!("Creating the new container...");
    //c.cc.
    ///////////////////////////////////////////////////////////////////////////////////////////////

    info!("Stopping the uVM...");
    let stop_req = StopVMRequest {
        VMID: conf::VMID.into(),
        TimeoutSeconds: conf::SERVER_SIDE_TIMEOUT_SEC,
        ..Default::default()
    };
    let _ = c
        .fcc
        .stop_vm(ctx, &stop_req)
        .await
        .with_context(|| "failed to stop uVM")?;

    Ok(())
}

#[instrument(level = Level::TRACE)]
async fn load_snapshot(args: LoadArgs) -> Result<()> {
    debug!("load_snapshot");

    Ok(())
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_writer(io::stderr)
        .with_env_filter(
            EnvFilter::from_default_env().add_directive(
                "cli05=trace"
                    .parse()
                    .with_context(|| "failed to parse filtering directive")?,
            ),
        )
        .with_thread_ids(true)
        .with_span_events(FmtSpan::NEW | FmtSpan::CLOSE)
        .try_init()
        .map_err(|err| anyhow!("failed to initialize tracing subscriber: {err}"))?;

    match argh::from_env::<Cli>().command {
        Command::CreateSnapshot(args) => create_snapshot(args).await,
        Command::LoadSnapshot(args) => load_snapshot(args).await,
    }
}

mod conf {
    use const_format::concatcp;
    use once_cell::sync::Lazy;
    use protobuf::MessageField;

    use crate::fc_ctrd::types::{
        FirecrackerNetworkInterface, IPConfiguration, StaticNetworkConfiguration,
    };

    // From: github.com/containerd/containerd@v1.6.8/namespaces/ttrpc.go
    pub(super) const TTRPC_HEADER_NAMESPACE_KEY: &str = "containerd-namespace-ttrpc";

    pub(super) const SERVER_SIDE_TIMEOUT_SEC: u32 = 5; // 5 sec
    pub(super) const CLIENT_SIDE_TIMEOUT_NS: i64 = 6 * 1000 * 1000 * 1000; // 6 sec

    pub(super) const NAMESPACE: &str = "default";
    pub(super) const CONTAINERD_ADDRESS: &str = "/run/firecracker-containerd/containerd.sock";
    pub(super) const CONTAINERD_TTRPC_ADDRESS: &str = concatcp!(CONTAINERD_ADDRESS, ".ttrpc");

    pub(super) const SNAPSHOTTER: &str = "devmapper";

    pub(super) const NGINX_IMAGE_NAME: &str = "docker.io/library/nginx:latest";

    pub(super) const VMID: &str = "testclient05-01";
    pub(super) const KERNEL_ARGS: &str = "i8042.nokbd i8042.noaux 8250.nr_uarts=0 ipv6.disable=1 noapic reboot=k panic=1 pci=off nomodules ro systemd.unified_cgroup_hierarchy=0 systemd.journald.forward_to_console systemd.unit=firecracker.target init=/sbin/overlay-init";

    pub(super) const SNAPSHOT_DIRNAME: &str = "/tmp/snapshots";
    pub(super) const SNAPSHOT_STATE_FILE_EXT: &str = "state";
    pub(super) const SNAPSHOT_MEMORY_FILE_EXT: &str = "memory";

    pub(super) const IP_GATEWAY: &str = "10.0.1.1";
    pub(super) const IP_ADDRESS: &str = "10.0.1.2";
    pub(super) const CIDR_MARK: &str = "/24";
    pub(super) const NGINX_URL: &str = concatcp!("http://", IP_ADDRESS, ":80");

    pub(super) static NETWORK_INTERFACE: Lazy<FirecrackerNetworkInterface> =
        Lazy::new(|| FirecrackerNetworkInterface {
            StaticConfig: MessageField::some(StaticNetworkConfiguration {
                MacAddress: "AA:FC:00:00:05:01".into(),
                HostDevName: "ckatsak.tap.01".into(),
                IPConfig: MessageField::some(IPConfiguration {
                    PrimaryAddr: concatcp!(IP_ADDRESS, CIDR_MARK).into(),
                    GatewayAddr: IP_GATEWAY.into(),
                    Nameservers: vec!["1.1.1.1".into(), "1.0.0.1".into()],
                    ..Default::default()
                }),
                ..Default::default()
            }),
            ..Default::default()
        });
}
