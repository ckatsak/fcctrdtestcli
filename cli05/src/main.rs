mod fc_ctrd;

use std::{io, path::PathBuf, time::Duration};

use anyhow::{anyhow, Context, Result};
use argh::FromArgs;
use const_format::concatcp;
use containerd_client::{
    services::v1::{
        container::Runtime,
        containers_client::ContainersClient,
        images_client::ImagesClient,
        snapshots::{snapshots_client::SnapshotsClient, PrepareSnapshotRequest},
        tasks_client::TasksClient,
        version_client::VersionClient,
        Container, CreateContainerRequest, CreateTaskRequest, DeleteContainerRequest,
        DeleteTaskRequest, GetImageRequest, Image, KillRequest, StartRequest,
    },
    with_namespace,
};
use prost_types::Any;
use tokio::{
    fs,
    time::{timeout, timeout_at, Instant},
};
use tonic::{transport::Channel, Request};
use tracing::{debug, info, instrument, trace, warn, Level};
use tracing_subscriber::{fmt::format::FmtSpan, EnvFilter};
use ttrpc::{asynchronous::Client as TtrpcClient, context};

use fc_ctrd::fccontrol_ttrpc::FirecrackerClient;
use fc_ctrd::firecracker::{
    CreateVMRequest, CreateVMSnapshotRequest, PauseVMRequest, ResumeVMRequest, StopVMRequest,
};

/// Command-line client to create or load VM snapshots.
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

/// Create a VM snapshot
#[derive(FromArgs, Debug)]
#[argh(subcommand, name = "snap")]
struct SnapArgs {
    /// image URL; e.g., "docker.io/library/nginx:latest"
    #[argh(option, short = 'i', default = "conf::NGINX_IMAGE_NAME.into()")]
    image: String,
}

/// Load a VM from a snapshot
#[derive(FromArgs, Debug)]
#[argh(subcommand, name = "load")]
struct LoadArgs {
    /// snapshot directory path; e.g., "/tmp/snapshots"
    #[argh(option, short = 'p', default = "PathBuf::from(conf::SNAPSHOT_DIRNAME)")]
    dir_path: PathBuf,
}

struct Client {
    //ctrd_chan: Channel,
    version: VersionClient<Channel>,
    snapshots: SnapshotsClient<Channel>,
    containers: ContainersClient<Channel>,
    images: ImagesClient<Channel>,
    tasks: TasksClient<Channel>,

    fcc: FirecrackerClient,
}

impl Client {
    #[instrument(level = Level::TRACE)]
    pub async fn new() -> Result<Self> {
        info!("Creating new containerd client...");
        let ctrd_chan = containerd_client::connect(conf::CONTAINERD_ADDRESS)
            .await
            .with_context(|| {
                format!(
                    "failed to connect to containerd through '{}'",
                    conf::CONTAINERD_ADDRESS
                )
            })?;
        let version = VersionClient::new(ctrd_chan.clone());
        let snapshots = SnapshotsClient::new(ctrd_chan.clone());
        let containers = ContainersClient::new(ctrd_chan.clone());
        let images = ImagesClient::new(ctrd_chan.clone());
        let tasks = TasksClient::new(ctrd_chan);

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
            //ctrd_chan,
            version,
            snapshots,
            containers,
            images,
            tasks,

            fcc,
        })
    }

    #[instrument(level = Level::TRACE, skip(self))]
    pub async fn get_image(&mut self, image_name: &str) -> Result<Image> {
        let req = GetImageRequest {
            name: image_name.into(),
        };
        self.images
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
            .version
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
    // Fetch the container image to be deployed inside the VM
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
    //let ctx = TtrpcContext { // use ttrpc::context::Context as TtrpcContext
    //    metadata: [(
    //        conf::TTRPC_HEADER_NAMESPACE_KEY.into(),
    //        vec![conf::NAMESPACE.into()],
    //    )]
    //    .into(),
    //    timeout_nano: conf::CLIENT_SIDE_TIMEOUT_NANOSEC, // 6 sec
    //};
    let mut ctx = context::with_timeout(conf::CLIENT_SIDE_TIMEOUT_NANOSEC); // 6 sec
    ctx.add(
        conf::TTRPC_HEADER_NAMESPACE_KEY.to_string(),
        conf::NAMESPACE.into(),
    );
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Create new VM
    info!("Creating new VM...");
    let create_vm_req = CreateVMRequest {
        VMID: conf::VMID.into(),
        KernelArgs: conf::KERNEL_ARGS.into(),
        NetworkInterfaces: vec![conf::NETWORK_INTERFACE.clone()],
        ContainerCount: 1,
        ExitAfterAllTasksDeleted: true,
        TimeoutSeconds: conf::SERVER_SIDE_TIMEOUT_SEC,
        ..Default::default()
    };
    let create_vm_resp = c
        .fcc
        .create_vm(ctx.clone(), &create_vm_req)
        .await
        .with_context(|| "failed to create new VM")?;
    debug!("fc-control responded: {create_vm_resp:?}");
    // TODO(ckatsak): Cleanup the new VM on exit
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Prepare a new container snapshot
    info!("Preparing new snapshot for a container...");
    let prep_snap_req = PrepareSnapshotRequest {
        snapshotter: conf::SNAPSHOTTER.into(),
        key: format!("{}-snap", conf::VMID),
        parent: "sha256:8b811a30cb94c227fb2ae61a2a1ec1e93381dbef06f9ea6b5c06df4f27651fed".into(),
        ..Default::default()
    };
    let prep_snap_resp = c
        .snapshots
        .prepare(with_namespace!(prep_snap_req, conf::NAMESPACE))
        .await
        .with_context(|| "failed to prepare new snapshot")?
        .into_inner();
    debug!("firecracker-containerd responded: {prep_snap_resp:?}");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Create the new container to run inside the VM
    info!("Creating the new container...");
    let spec = oci_templates::load(conf::NAMESPACE, conf::VMID, &image.name)
        .await
        .with_context(|| "failed to load OCI spec")?;
    let create_ctr_req = CreateContainerRequest {
        container: Some(Container {
            id: conf::VMID.into(),
            image: args.image,
            runtime: Some(Runtime {
                name: "aws.firecracker".into(),
                options: None,
            }),
            snapshotter: conf::SNAPSHOTTER.into(),
            snapshot_key: format!("{}-snap", conf::VMID),
            spec: Some(Any {
                type_url: "types.containerd.io/opencontainers/runtime-spec/1/Spec".into(),
                value: serde_json::to_vec(&spec)
                    .with_context(|| "failed to JSON-(re-)marshal OCI spec")?,
            }),
            ..Default::default()
        }),
    };
    let create_ctr_resp = c
        .containers
        .create(with_namespace!(create_ctr_req, conf::NAMESPACE))
        .await
        .with_context(|| "failed to create new container")?
        .into_inner();
    debug!("firecracker-containerd responded: {create_ctr_resp:?}");
    let Some(container) = create_ctr_resp.container else {
        return Err(anyhow!("container field in CreateContainerResponse is None"))
    };
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Creating new task
    info!("Creating new task...");
    let create_task_req = CreateTaskRequest {
        container_id: container.id.clone(),
        rootfs: prep_snap_resp.mounts,
        //terminal: false,
        //stdin: "".to_string(),
        //stdout: "".to_string(),
        //stderr: "".to_string(),
        ..Default::default()
    };
    let create_task_resp = c
        .tasks
        .create(with_namespace!(create_task_req, conf::NAMESPACE))
        .await
        .with_context(|| "")?
        .into_inner();
    debug!("firecracker-containerd responded: {create_task_resp:?}");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Start the task
    info!("Starting the new task...");
    let start_task_req = StartRequest {
        container_id: container.id.clone(),
        ..Default::default()
    };
    let start_task_resp = c
        .tasks
        .start(with_namespace!(start_task_req, conf::NAMESPACE))
        .await
        .with_context(|| "failed to start task")?
        .into_inner();
    debug!("firecracker-containerd responded: {start_task_resp:?}");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // HTTP GET
    let http = reqwest::Client::new();
    const MAX_RETRIES: u32 = 100;
    const TICK: Duration = Duration::from_millis(10);

    let mut attempts = 0;
    let hresp = loop {
        let deadline = Instant::now() + TICK;
        match timeout_at(deadline, http.get(conf::NGINX_URL).send()).await {
            Ok(Ok(resp)) => break Ok(resp),
            Ok(_http_err) => tokio::time::sleep_until(deadline).await,
            Err(_timeout_err) => {}
        };
        attempts += 1;
        match attempts {
            MAX_RETRIES => {
                break Err(anyhow!(
                    "failed to HTTP GET to the function inside the VM after {}",
                    humantime::format_duration(TICK * MAX_RETRIES)
                ))
            }
            attempts if attempts % 10 == 0 => warn!(
                "failing to HTTP GET to the function inside the VM after {}",
                humantime::format_duration(TICK * attempts)
            ),
            _ => (/* continue */),
        }
    }?; // FIXME(ckatsak): task, container and VM are still up!
    let t_end_1 = Instant::now();
    debug!("workload responded with {}", hresp.status());
    trace!(
        "workload's response body:\n{}",
        hresp
            .text()
            .await
            .with_context(|| "failed to render HTTP response body as text")?
    );
    let t_end_2 = Instant::now();
    info!(
        "Workload's response latency when creating a new VM: {} (or maybe just {})",
        humantime::format_duration(t_end_2 - t_start),
        humantime::format_duration(t_end_1 - t_start),
    );
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    fs::create_dir_all(conf::SNAPSHOT_DIRNAME)
        .await
        .with_context(|| format!("failed to mkdir {:?}", conf::SNAPSHOT_DIRNAME))?;
    info!("Sleeping for 10 seconds before creating the snapshot for you to play...");
    tokio::time::sleep(Duration::from_secs(10)).await;
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Pause the VM
    info!("Pausing the VM...");
    let pause_req = PauseVMRequest {
        VMID: conf::VMID.into(),
        ..Default::default()
    };
    let _ = c
        .fcc
        .pause_vm(ctx.clone(), &pause_req)
        .await
        .with_context(|| "failed to pause the VM");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Create the new VM snapshot from the paused VM
    info!("Creating a new VM snapshot...");
    let create_snap_req = CreateVMSnapshotRequest {
        VMID: conf::VMID.into(),
        SnapshotPath: format!(
            "{}/{}.{}",
            conf::SNAPSHOT_DIRNAME,
            conf::VMID,
            conf::SNAPSHOT_STATE_FILE_EXT
        ),
        MemFilePath: format!(
            "{}/{}.{}",
            conf::SNAPSHOT_DIRNAME,
            conf::VMID,
            conf::SNAPSHOT_MEMORY_FILE_EXT
        ),
        ..Default::default()
    };
    let _ = c
        .fcc
        .create_vm_snapshot(ctx.clone(), &create_snap_req)
        .await
        .with_context(|| "failed to create VM snapshot")?;
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Resume the VM
    info!("Resuming the VM...");
    let resume_req = ResumeVMRequest {
        VMID: conf::VMID.into(),
        ..Default::default()
    };
    let _ = c
        .fcc
        .resume_vm(ctx.clone(), &resume_req)
        .await
        .with_context(|| "failed to resume the VM");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Make sure the VM is still functional after resuming it
    match timeout(Duration::from_millis(500), http.get(conf::NGINX_URL).send()).await {
        Ok(Ok(_)) => info!("The function looks functional after resuming the VM!"),
        Ok(Err(er)) => warn!("Function looks dysfunctional after resuming the VM: {}", er),
        Err(timeout_err) => warn!("Function timed out after resuming the VM: {}", timeout_err),
    };

    info!("Sleeping for 10 seconds before creating the snapshot for you to play...");
    tokio::time::sleep(Duration::from_secs(10)).await;
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Kill the task
    info!("Killing the task...");
    let kill_task_req = KillRequest {
        container_id: container.id.clone(),
        signal: 9, // SIGKILL
        ..Default::default()
    };
    c.tasks
        .kill(with_namespace!(kill_task_req, conf::NAMESPACE))
        .await
        .with_context(|| "failed to kill task")? // FIXME: Do not exit on error?
        // (firecracker-containerd should be removing the container after the task has been killed,
        // but we should do that manually if kill fails
        .into_inner();
    info!("Task has been killed successfully!");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Delete the task
    // TODO(ckatsak): This should probably be some kind of guard?
    let del_task_req = DeleteTaskRequest {
        container_id: container.id.clone(),
    };
    let del_task_resp = c
        .tasks
        .delete(with_namespace!(del_task_req, conf::NAMESPACE))
        .await
        .with_context(|| "failed to delete task")? // FIXME: Do not exit on error?
        .into_inner();
    debug!("firecracker-containerd responded: {del_task_resp:?}");
    info!("Container has been deleted successfully!");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Delete the container
    // TODO(ckatsak): This should probably be some kind of guard?
    info!("Deleting container '{}'...", container.id);
    let del_ctr_req = DeleteContainerRequest { id: container.id };
    let del_ctr_resp = c
        .containers
        .delete(with_namespace!(del_ctr_req, conf::NAMESPACE))
        .await
        .with_context(|| "failed to delete the container")?; // FIXME: Do not exit on error?
    debug!("firecracker-containerd responded: {del_ctr_resp:?}");
    info!("Container has been deleted successfully!");
    ///////////////////////////////////////////////////////////////////////////////////////////////

    ///////////////////////////////////////////////////////////////////////////////////////////////
    // Stop the VM
    // TODO(ckatsak): This should probably be some kind of guard?
    info!("Stopping the VM...");
    let stop_req = StopVMRequest {
        VMID: conf::VMID.into(),
        TimeoutSeconds: conf::SERVER_SIDE_TIMEOUT_SEC,
        ..Default::default()
    };
    let _ = c
        .fcc
        .stop_vm(ctx, &stop_req)
        .await
        .with_context(|| "failed to stop VM")?;
    info!("VM has been stopped successfully!");
    ///////////////////////////////////////////////////////////////////////////////////////////////

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

mod oci_templates {
    use std::path::Path;

    use anyhow::{Context, Result};
    use oci_spec::runtime::Spec;
    use tokio::fs;

    pub(super) async fn load(ns: &str, vm_id: &str, image_name: &str) -> Result<Spec> {
        let mut path = Path::new("spec_templates").join(
            image_name
                .split('/')
                .last()
                .expect("image name should not be None"),
        );
        path.set_extension("json");
        let template = fs::read_to_string(&path)
            .await
            .with_context(|| format!("failed to read image template from '{}'", path.display()))?;
        let template = template
            .replace("@CONTAINERD_NAMESPACE", ns)
            .replace("@VMID", vm_id);
        serde_json::from_str(&template).with_context(|| "failed to deserialize JSON spec")
    }
}

pub mod conf {
    use const_format::concatcp;
    use once_cell::sync::Lazy;
    use protobuf::MessageField;

    use crate::fc_ctrd::types::{
        FirecrackerNetworkInterface, IPConfiguration, StaticNetworkConfiguration,
    };

    // According to github.com/containerd/containerd@v1.6.8/namespaces/ttrpc.go
    pub(super) const TTRPC_HEADER_NAMESPACE_KEY: &str = "containerd-namespace-ttrpc";

    pub(super) const SERVER_SIDE_TIMEOUT_SEC: u32 = 5; // 5 sec
    pub(super) const CLIENT_SIDE_TIMEOUT_NANOSEC: i64 = 7 * 1000 * 1000 * 1000; // 7 sec

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

mod oci {
    //! So... constructing a new OCI spec proved to be more of a pain in the ass than I expected.
    //! Therefore, screw this for now: we'll just generate it via the containerd Go client and load
    //! it here when needed.

    use std::{collections::HashMap, path::Path};

    use anyhow::{Context, Result};
    use oci_spec::runtime::{
        Capability, LinuxBuilder, LinuxCapabilitiesBuilder, LinuxDeviceCgroupBuilder,
        LinuxNamespaceBuilder, LinuxNamespaceType, LinuxResourcesBuilder, LinuxRlimitBuilder,
        LinuxRlimitType, MountBuilder, ProcessBuilder, RootBuilder, Spec, SpecBuilder, UserBuilder,
    };

    pub(super) fn with_vmid(spec: &mut Spec, vm_id: &str) -> Result<()> {
        let mut new_annotations = if let Some(annotations) = spec.annotations().as_ref() {
            annotations.clone()
        } else {
            HashMap::with_capacity(1)
        };
        let _ = new_annotations.insert("aws.firecracker.vm.id".into(), vm_id.into());
        let _ = spec.set_annotations(Some(new_annotations));
        Ok(())
    }

    pub(super) fn with_vm_network(spec: &mut Spec) -> Result<()> {
        let linux = spec
            .linux()
            .as_ref()
            .expect("Spec's 'Linux' field should not be None");
        let namespaces = linux
            .namespaces()
            .as_ref()
            .expect("Spec's 'Linux.Namespaces' field should not be None");
        let new_namespaces = namespaces
            .clone()
            .into_iter()
            .filter(|ns| {
                !matches!(
                    ns.typ(),
                    LinuxNamespaceType::Network | LinuxNamespaceType::Uts
                )
            })
            .collect();
        let mut new_linux = linux.clone();
        let _ = new_linux.set_namespaces(Some(new_namespaces));
        let _ = spec.set_linux(Some(new_linux));

        let mounts = spec
            .mounts()
            .as_ref()
            .expect("Spec's 'Mounts' field should not be None");
        let mut new_mounts = mounts.clone();
        new_mounts.extend([
            MountBuilder::default()
                .destination("/etc/resolv.conf")
                .typ("bind")
                .source("/etc/resolv.conf")
                .options(["rbind".into(), "ro".into()])
                .build()
                .with_context(|| "failed to build OCI (resolv.conf) Mount")?,
            MountBuilder::default()
                .destination("/etc/hosts")
                .typ("bind")
                .source("/etc/hosts")
                .options(["rbind".into(), "ro".into()])
                .build()
                .with_context(|| "failed to build OCI (hosts) Mount")?,
        ]);
        let _ = spec.set_mounts(Some(new_mounts));

        Ok(())
    }

    /// Arguments:
    /// - `ns` is the containerd namespace;
    /// - `id` is containerd's `Container.ID` (which is probably just `VMID` for us).
    pub(super) fn generate_default_unix_spec(ns: &str, id: &str) -> Result<Spec> {
        let caps = [
            Capability::Chown,
            Capability::DacOverride,
            Capability::Fsetid,
            Capability::Fowner,
            Capability::Mknod,
            Capability::NetRaw,
            Capability::Setgid,
            Capability::Setuid,
            Capability::Setfcap,
            Capability::Setpcap,
            Capability::NetBindService,
            Capability::SysChroot,
            Capability::Kill,
            Capability::AuditWrite,
        ];
        SpecBuilder::default()
            // I found the OCI version by following the code path in containerd's `WithNewSpec`:
            // find the `populateDefaultUnixSpec()` where the `Version` field is populated like:
            // `Version: specs.Version`. By checking that containerd's dependencies (@ go.mod) we
            // find the correct version of the opencontainers/runtime-spec package, and then check
            // the constants in specs-go/version.go over there.
            // My current version of containerd v1.6.8 uses "1.0.2-dev", but this might have
            // changed in containerd v1.6.9.
            .version("1.0.2-dev")
            // `Root::default()` can also set `path = "rootfs"` (which is the the same as
            // `defaultRootfsPath` in containerd), and set `ro = true`.
            .root(
                RootBuilder::default()
                    .path("rootfs")
                    .readonly(true)
                    .build()
                    .with_context(|| "failed to build OCI root")?,
            )
            .process(
                ProcessBuilder::default()
                    .cwd("/")
                    .no_new_privileges(true)
                    .user(
                        UserBuilder::default()
                            .uid(0u32)
                            .gid(0u32)
                            .build()
                            .with_context(|| "failed to build OCI user")?,
                    )
                    .capabilities(
                        LinuxCapabilitiesBuilder::default()
                            .bounding(caps)
                            .permitted(caps)
                            .effective(caps)
                            .build()
                            .with_context(|| "failed to build OCI LinuxCapabilities")?,
                    )
                    .rlimits([LinuxRlimitBuilder::default()
                        .typ(LinuxRlimitType::RlimitNofile)
                        .hard(1024u64)
                        .soft(1024u64)
                        .build()
                        .with_context(|| "failed to build OCI LinuxRlimit")?])
                    .build()
                    .with_context(|| "failed to build OCI process")?,
            )
            .linux(
                LinuxBuilder::default()
                    .masked_paths([
                        "/proc/acpi".into(),
                        "/proc/asound".into(),
                        "/proc/kcore".into(),
                        "/proc/keys".into(),
                        "/proc/latency_stats".into(),
                        "/proc/timer_list".into(),
                        "/proc/timer_stats".into(),
                        "/proc/sched_debug".into(),
                        "/sys/firmware".into(),
                        "/proc/scsi".into(),
                    ])
                    .readonly_paths([
                        "/proc/bus".into(),
                        "/proc/fs".into(),
                        "/proc/irq".into(),
                        "/proc/sys".into(),
                        "/proc/sysrq-trigger".into(),
                    ])
                    .cgroups_path(Path::new("/").join(ns).join(id))
                    .resources(
                        LinuxResourcesBuilder::default()
                            .devices([LinuxDeviceCgroupBuilder::default()
                                .allow(false)
                                .access("rwm")
                                .build()
                                .with_context(|| "failed to build OCI LinuxDeviceCgroup")?])
                            .build()
                            .with_context(|| "failed to build OCI LinuxResources")?,
                    )
                    .namespaces([
                        LinuxNamespaceBuilder::default()
                            .typ(LinuxNamespaceType::Pid)
                            .build()
                            .with_context(|| "failed to build OCI (PID) LinuxNamespace")?,
                        LinuxNamespaceBuilder::default()
                            .typ(LinuxNamespaceType::Ipc)
                            .build()
                            .with_context(|| "failed to build OCI (IPC) LinuxNamespace")?,
                        LinuxNamespaceBuilder::default()
                            .typ(LinuxNamespaceType::Uts)
                            .build()
                            .with_context(|| "failed to build OCI (UTS) LinuxNamespace")?,
                        LinuxNamespaceBuilder::default()
                            .typ(LinuxNamespaceType::Mount)
                            .build()
                            .with_context(|| "failed to build OCI (mount) LinuxNamespace")?,
                        LinuxNamespaceBuilder::default()
                            .typ(LinuxNamespaceType::Network)
                            .build()
                            .with_context(|| "failed to build OCI (network) LinuxNamespace")?,
                    ])
                    .build()
                    .with_context(|| "failed to build OCI Linux")?,
            )
            .mounts([
                MountBuilder::default()
                    .destination("/proc")
                    .typ("proc")
                    .source("proc")
                    .options(["nosuid".into(), "noexec".into(), "nodev".into()])
                    .build()
                    .with_context(|| "failed to build OCI (proc) Mount")?,
                MountBuilder::default()
                    .destination("/dev")
                    .typ("tmpfs")
                    .source("tmpfs")
                    .options([
                        "nosuid".into(),
                        "strictatime".into(),
                        "mode=755".into(),
                        "size=65536k".into(),
                    ])
                    .build()
                    .with_context(|| "failed to build OCI Mount")?,
                MountBuilder::default()
                    .destination("/dev/pts")
                    .typ("devpts")
                    .source("devpts")
                    .options([
                        "nosuid".into(),
                        "noexec".into(),
                        "newinstance".into(),
                        "ptmxmode=0666".into(),
                        "mode=0620".into(),
                        "gid=5".into(),
                    ])
                    .build()
                    .with_context(|| "failed to build OCI Mount")?,
                MountBuilder::default()
                    .destination("/dev/shm")
                    .typ("tmpfs")
                    .source("shm")
                    .options([
                        "nosuid".into(),
                        "noexec".into(),
                        "nodev".into(),
                        "mode=1777".into(),
                        "size=65536k".into(),
                    ])
                    .build()
                    .with_context(|| "failed to build OCI Mount")?,
                MountBuilder::default()
                    .destination("/dev/mqueue")
                    .typ("mqueue")
                    .source("mqueue")
                    .options(["nosuid".into(), "noexec".into(), "nodev".into()])
                    .build()
                    .with_context(|| "failed to build OCI Mount")?,
                MountBuilder::default()
                    .destination("/sys")
                    .typ("sysfs")
                    .source("sysfs")
                    .options([
                        "nosuid".into(),
                        "noexec".into(),
                        "nodev".into(),
                        "ro".into(),
                    ])
                    .build()
                    .with_context(|| "failed to build OCI Mount")?,
                MountBuilder::default()
                    .destination("/run")
                    .typ("tmpfs")
                    .source("tmpfs")
                    .options([
                        "nosuid".into(),
                        "strictatime".into(),
                        "mode=755".into(),
                        "size=65536k".into(),
                    ])
                    .build()
                    .with_context(|| "failed to build OCI Mount")?,
            ])
            .build()
            .with_context(|| "failed to build OCI spec")
    }
}
