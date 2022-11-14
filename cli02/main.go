package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"

	fcclient "github.com/firecracker-microvm/firecracker-containerd/firecracker-control/client"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/firecracker-microvm/firecracker-containerd/runtime/firecrackeroci"
)

const (
	defaultNamespace              = namespaces.Default
	defaultContainerdAddress      = "/run/firecracker-containerd/containerd.sock"
	defaultContainerdTTRPCAddress = defaultContainerdAddress + ".ttrpc"

	snapshotter = "devmapper"

	nginxImageName = "docker.io/library/nginx:latest"

	vmID       = "testclient02-01"
	kernelArgs = "i8042.nokbd i8042.noaux 8250.nr_uarts=0 ipv6.disable=1 noapic reboot=k panic=1 pci=off nomodules ro systemd.unified_cgroup_hierarchy=0 systemd.journald.forward_to_console systemd.unit=firecracker.target init=/sbin/overlay-init"

	defaultSnapshotDirname       = "/tmp/snapshots"
	defaultSnapshotStateFileExt  = "state"
	defaultSnapshotMemoryFileExt = "memory"

	defaultIPGateway = "10.0.1.1"
	defaultIPAddr    = "10.0.1.2"
	defaultCIDRMask  = "/24"
	defaultNginxURL  = "http://" + defaultIPAddr + ":80"
)

var (
	defaultNetworkInterface = &proto.FirecrackerNetworkInterface{
		StaticConfig: &proto.StaticNetworkConfiguration{
			MacAddress:  "AA:FC:00:00:05:01",
			HostDevName: "ckatsak.tap.01",
			IPConfig: &proto.IPConfiguration{
				PrimaryAddr: defaultIPAddr + defaultCIDRMask,
				GatewayAddr: defaultIPGateway,
				Nameservers: []string{"1.1.1.1", "1.0.0.1"},
			},
		},
	}

	log *logrus.Entry
)

func init() {
	log = logrus.New().WithFields(logrus.Fields{
		"namespace": defaultNamespace,
		"vmID":      vmID,
	})
	log.Logger.SetLevel(logrus.TraceLevel)
	log.Logger.SetFormatter(&logrus.TextFormatter{
		TimestampFormat:        time.RFC3339Nano,
		FullTimestamp:          true,
		DisableLevelTruncation: true,
	})
}

type Client struct {
	cc  *containerd.Client
	fcc *fcclient.Client
}

func NewClient(containerdAddress, containerdTTRPCAddress string) (_ *Client, err error) {
	log := log.WithField("func", "NewClient()")

	log.Infof("Creating new containerd client...")
	client, err := containerd.New(containerdAddress)
	if err != nil {
		log.WithError(err).Errorf("error creating containerd client")
		return
	}
	defer func() {
		if err != nil {
			log.Infof("Closing containerd client...")
			if err := client.Close(); err != nil {
				log.WithError(err).Warnf("error closing containerd client")
			}
		}
	}()

	log.Infof("Creating new firecracker-containerd client...")
	fcClient, err := fcclient.New(containerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("fcclient.New() failed")
		return
	}

	return &Client{
		cc:  client,
		fcc: fcClient,
	}, nil
}

func (c *Client) Close() {
	log := log.WithField("func", "(*Client).Close()")

	if err := c.fcc.Close(); err != nil {
		log.WithError(err).Warnf("failed to close firecracker-control client")
	}
	if err := c.cc.Close(); err != nil {
		log.WithError(err).Warnf("failed to close containerd client")
	}
}

// getImage returns the containerd.Image associated with the provided image name.
//
// Also see:
// - https://github.com/estesp/examplectr/blob/master/examplectr.go
// - https://github.com/containerd/containerd/blob/release/1.6/docs/getting-started.md
// - firecracker-containerd examples (taskworkflow and remote-snapshotter)
// - vHive
func (c *Client) getImage(ctx context.Context, imageName string) (image containerd.Image, err error) {
	log := log.WithField("func", "(*Client).getImage()")

	if image, err = c.cc.GetImage(ctx, imageName); err == nil {
		log.Infof("Image found through (*containerd.Client).GetImage()")
		return
	}

	// if the image isn't already in our namespaced context, then pull it
	log.WithError(err).Infof("Pulling image")
	if image, err = c.cc.Pull(
		ctx,
		imageName,
		containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(snapshotter),
	); err != nil {
		err = fmt.Errorf("failed to (*containerd.Client).Pull('%s'): %v", imageName, err)
		log.WithError(err).Warn()
	}
	return
}

func createSnapshot(imageName string) {
	var (
		log = log.WithFields(logrus.Fields{
			"mode":  "createSnapshot",
			"image": imageName,
		})
		err error
	)

	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)

	c, err := NewClient(defaultContainerdAddress, defaultContainerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("failed to create client")
		return
	}
	defer c.Close()

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Version info for containerd.Client
	if v, err := c.cc.Version(ctx); err != nil {
		log.WithError(err).Errorf("failed to get containerd's version information")
		return
	} else {
		log.Infof("(*containerd.Client).Version() returned %#v", v)
	}
	log.Infof("(*containerd.Client).Runtime() returned '%s'", c.cc.Runtime())
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Fetch the container image to be deployed inside the uVM
	log.Infof("Fetching the container image...")
	image, err := c.getImage(ctx, imageName)
	if err != nil {
		log.WithError(err).Errorf("failed to getImage()")
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	tStart := time.Now()

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create new uVM
	log.Infof("Creating new uVM...")
	createReq := &proto.CreateVMRequest{
		VMID:                     vmID,
		TimeoutSeconds:           30,
		NetworkInterfaces:        []*proto.FirecrackerNetworkInterface{defaultNetworkInterface},
		KernelArgs:               kernelArgs,
		ContainerCount:           1,
		ExitAfterAllTasksDeleted: true,
	}
	resp, err := c.fcc.CreateVM(ctx, createReq)
	if err != nil {
		log.WithError(err).Errorf("error creating new VM")
		return
	}
	defer func() {
		log.Infof("Stopping uVM...")
		if _, err := c.fcc.StopVM(ctx, &proto.StopVMRequest{VMID: vmID, TimeoutSeconds: 5}); err != nil {
			log.WithError(err).Warnf("failed to StopVM() on deferred call")
		}
	}()
	log.Tracef("fcClient.CreateVM() responded: %#v", resp)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	//log.Tracef("TEST: Sleeping for 10 seconds...")
	//time.Sleep(10 * time.Second)

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create the new container to run inside the uVM
	log.Infof("Creating the new container...")
	container, err := c.cc.NewContainer(
		ctx,
		vmID,
		containerd.WithSnapshotter(snapshotter),
		containerd.WithNewSnapshot(vmID+"-snap", image),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			firecrackeroci.WithVMID(vmID),
			firecrackeroci.WithVMNetwork,
		),
		containerd.WithRuntime("aws.firecracker", nil),
	)
	if err != nil {
		log.WithError(err).Errorf("failed to create NewContainer()")
		return
	}
	defer func() {
		log.Infof("Deleting container '%s'...", container.ID())
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			log.WithError(err).Warnf("failed to delete container '%s' with snapshot-cleanup on deferred call", container.ID())
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	//log.Tracef("TEST: Sleeping for 10 seconds...")
	//time.Sleep(10 * time.Second)

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a new task from the container we just defined, to run inside the uVM
	log.Infof("Creating the new task...")
	var task containerd.Task
	task, err = container.NewTask(ctx, cio.NullIO)
	if err != nil {
		log.WithError(err).Errorf("failed to create new task")
		return
	}
	defer func() {
		log.Infof("Deleting task...")
		if exitStatus, err := task.Delete(ctx); err != nil {
			log.WithError(err).Warnf("failed to delete task on deferred call")
		} else {
			log.Infof("Deleted task's exit status on deferred call: %#v", exitStatus)
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	//log.Tracef("TEST: Sleeping for 10 seconds...")
	//time.Sleep(10 * time.Second)

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Asynchronously wait for the task to complete ... for now (?)
	log.Infof("Creating the new task's exit channel...")
	var esCh <-chan containerd.ExitStatus
	esCh, err = task.Wait(ctx) // esCh: (<-chan containerd.ExitStatus)
	if err != nil {
		log.WithError(err).Errorf("failed to (*containerd.Task).Wait()")
		return
	}
	go func() {
		log := log.WithField("func", "goroutine:task.Wait()")
		log.Infof("Asynchronously waiting for the task to complete...")
		es := <-esCh
		log.Infof("Task completed! Exit status: %#v", es)
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Start the task
	log.Infof("Starting the new task...")
	if err = task.Start(ctx); err != nil {
		log.WithError(err).Errorf("failed to start the new Task")
		return
	}
	defer func() {
		// Only SIGKILL if we exit with an error?
		//if err != nil {
		log.Infof("Sending a SIGKILL to the task...")
		if err := task.Kill(ctx, syscall.SIGKILL); err != nil {
			log.WithError(err).Warnf("failed to send a SIGKILL to the task on deferred call")
		}
		//}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	//log.Tracef("TEST: Sleeping for 10 seconds...")
	//time.Sleep(10 * time.Second)

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// HTTP GET
	log.Infof("Issuing HTTP GET request to the server inside the uVM...")
	const (
		maxRetries = 100
		tick       = 10 * time.Millisecond
	)
	var (
		throttle     = time.Tick(tick)
		retries      = 0
		workloadResp *http.Response
	)
	for ; retries < maxRetries; retries++ {
		var err error

		<-throttle
		if workloadResp, err = http.Get(defaultNginxURL); err == nil {
			break
		}
		if retries%10 == 0 && retries != 0 {
			log.WithError(err).Warnf("failed to HTTP GET to the function inside the uVM after %v", time.Duration(retries)*tick)
		}
	}
	tEnd := time.Since(tStart)
	if err != nil {
		log.WithError(err).Errorf("failed to HTTP GET to the function inside the uVM after %v", maxRetries*tick)
	}
	defer func() {
		if err := workloadResp.Body.Close(); err != nil {
			log.WithError(err).Warnf("failed to close reader for HTTP response's body")
		}
	}()
	log.Tracef("workload's response: %#v", workloadResp)
	log.Infof("Latency when creating a new VM: %v", tEnd)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Print out container's OCI runtime spec
	if spec, err := container.Spec(ctx); err != nil {
		log.WithError(err).Warnf("failed to retrieve container's OCI runtime spec")
	} else {
		if marshalledSpec, err := json.MarshalIndent(spec, "", "    "); err != nil {
			log.WithError(err).Warnf("failed to JSON-marshal container's OCI runtime spec")
		} else {
			log.Tracef("New container's OCI runtime spec:\n%s", marshalledSpec)
		}
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	if err = os.MkdirAll(defaultSnapshotDirname, 0775); err != nil {
		log.WithError(err).Errorf("failed to mkdir('%s') for snapshots", defaultSnapshotDirname)
		return
	}
	log.Infof("Sleeping for 10 seconds before creating the snapshot for you to play...")
	time.Sleep(10 * time.Second)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Pause the uVM
	log.Infof("Pausing the uVM...")
	if _, err = c.fcc.PauseVM(ctx, &proto.PauseVMRequest{VMID: vmID}); err != nil {
		log.WithError(err).Errorf("failed to pause the uVM")
		return
	}
	defer func() {
		// Only resume if we exit with an error?
		//if err != nil {
		log.Infof("Resuming the paused uVM...")
		if _, err := c.fcc.ResumeVM(ctx, &proto.ResumeVMRequest{VMID: vmID}); err != nil {
			log.WithError(err).Warnf("failed to resume the uVM on deferred call")
		} else {
			log.Infof("uVM has been resumed successfully!")

			log.Infof("Sleeping for 10sec for you to play...")
			time.Sleep(10 * time.Second)
		}
		//}
	}()
	log.Infof("uVM has been paused successfully!")
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a new uVM snapshot from the paused uVM
	log.Infof("Creating a new uVM snapshot...")
	snapReq := &proto.CreateVMSnapshotRequest{
		VMID:         vmID,
		SnapshotPath: filepath.Join(defaultSnapshotDirname, fmt.Sprintf("%s.%s", vmID, defaultSnapshotStateFileExt)),
		MemFilePath:  filepath.Join(defaultSnapshotDirname, fmt.Sprintf("%s.%s", vmID, defaultSnapshotMemoryFileExt)),
	}
	if _, err = c.fcc.CreateVMSnapshot(ctx, snapReq); err != nil {
		log.WithError(err).Errorf("failed to create uVM snapshot")
		return
	}
	log.Infof("uVM's snapshot has been created successfully!")
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	// Return, allowing the deferred calls to attempt to resume the uVM and cleanup the task and the container
}

func loadSnapshot(snapshotDirPath string) {
	var (
		err error
		log = log.WithFields(logrus.Fields{
			"mode": "loadSnapshot",
		})
	)

	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)

	c, err := NewClient(defaultContainerdAddress, defaultContainerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("failed to create client")
		return
	}
	defer c.Close()

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Version info for containerd.Client
	if v, err := c.cc.Version(ctx); err != nil {
		log.WithError(err).Errorf("failed to get containerd's version information")
		return
	} else {
		log.Infof("(*containerd.Client).Version() returned %#v", v)
	}
	log.Infof("(*containerd.Client).Runtime() returned '%s'", c.cc.Runtime())
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	tStart := time.Now()

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Load uVM from the snapshot
	log.Infof("Loading uVM from snapshot...")
	loadReq := &proto.LoadVMSnapshotRequest{
		VMID:         vmID,
		SnapshotPath: filepath.Join(snapshotDirPath, fmt.Sprintf("%s.%s", vmID, defaultSnapshotStateFileExt)),
		MemFilePath:  filepath.Join(snapshotDirPath, fmt.Sprintf("%s.%s", vmID, defaultSnapshotMemoryFileExt)),
		ResumeVM:     true,
	}
	if _, err = c.fcc.LoadVMSnapshot(ctx, loadReq); err != nil {
		log.WithError(err).Errorf("failed to load uVM from the snapshot")
		return
	}
	defer func() {
		log.Infof("Stopping uVM...")
		if _, err := c.fcc.StopVM(ctx, &proto.StopVMRequest{VMID: vmID, TimeoutSeconds: 5}); err != nil {
			log.WithError(err).Warnf("failed to stop uVM")
		}
	}()
	log.Infof("uVM loaded from snapshot successfully!")
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// HTTP GET
	log.Infof("Issuing HTTP GET request to the server inside the uVM...")
	workloadResp, err := http.Get(defaultNginxURL)
	tEnd := time.Since(tStart)
	if err != nil {
		log.WithError(err).Errorf("failed to (immediately) HTTP GET to the function in the uVM")
		log.WithError(err).Debugf("Sleeping for 5 seconds before exiting, to let that sink in...")
		time.Sleep(5 * time.Second)
		return
	}
	defer func() {
		if err := workloadResp.Body.Close(); err != nil {
			log.WithError(err).Warnf("failed to close reader for HTTP response's body")
		}
	}()
	log.Tracef("workload's response: %#v", workloadResp)
	log.Infof("Latency when loading uVM from snapshot: %v", tEnd)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	log.Infof("Sleeping for 10 seconds for you to play...")
	time.Sleep(10 * time.Second)

	// Return, allowing the deferred calls to attempt to stop the uVM
}

func main() {
	switch len(os.Args) {
	case 3:
		switch os.Args[1] {
		case "load":
			// <cli> <command> <snapshot_dir_path>
			loadSnapshot(os.Args[2])
		case "snap":
			// <cli> <command> <image>
			createSnapshot(os.Args[2])
		}
	case 2:
		// <cli> <command>
		switch os.Args[1] {
		case "load":
			loadSnapshot(defaultSnapshotDirname)
		case "snap":
			createSnapshot(nginxImageName)
		}
	}
}
