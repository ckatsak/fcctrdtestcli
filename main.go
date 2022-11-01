package main

import (
	"context"
	"fmt"
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
	defaultNamespace              = "default"
	defaultContainerdAddress      = "/run/firecracker-containerd/containerd.sock"
	defaultContainerdTTRPCAddress = defaultContainerdAddress + ".ttrpc"

	snapshotter = "devmapper"

	imageName = "docker.io/library/debian:bullseye"

	vmID = "testclient01-01"

	defaultSnapshotDirname       = "/tmp/snap-testclient01"
	defaultSnapshotStateFileExt  = "state"
	defaultSnapshotMemoryFileExt = "memory"
)

var (
	log *logrus.Entry
)

func init() {
	log = logrus.New().WithFields(logrus.Fields{
		"namespace": defaultNamespace,
		"vmID":      vmID,
		"imageName": imageName,
	})
	log.Logger.SetLevel(logrus.TraceLevel)
	log.Logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:          true,
		DisableLevelTruncation: true,
	})
}

type Client struct {
	cc  *containerd.Client
	fcc *fcclient.Client
}

func NewClient(containerdAddress, containerdTTRPCAddress string) (ret *Client, err error) {
	log := log.WithField("func", "NewClient()")

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create containerd client
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
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create firecracker-containerd fc-control client
	log.Infof("Creating new firecracker-containerd client...")
	fcClient, err := fcclient.New(containerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("fcclient.New() failed")
		return
	}
	defer func() {
		if err != nil {
			log.Infof("Closing firecracker-containerd client...")
			if err := fcClient.Close(); err != nil {
				log.WithError(err).Warnf("error closing firecracker-control client")
			}
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	return &Client{
		cc:  client,
		fcc: fcClient,
	}, nil
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

// vanilla simply creates a new VM and an interactive container inside it.
//
// This should work with vanilla firecracker-containerd (commit `e177574`).
func vanilla() {
	log := log.WithField("func", "vanilla")
	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)

	c, err := NewClient(defaultContainerdAddress, defaultContainerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("failed to create client")
		return
	}

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
	// Bit more relevant containerd.Client test... FIXME(ckatsak): to be removed
	log.Debugf("Bit-more-relevant-but-still-random testing for containerd client...")
	images, err := c.cc.ImageService().List(ctx)
	if err != nil {
		log.WithError(err).Errorf("client.ImageService().List() failed")
		return
	}
	log.Debugf("images = %#v", images)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create new uVM
	log.Infof("Creating new uVM...")
	req := &proto.CreateVMRequest{
		VMID:           vmID,
		TimeoutSeconds: 20, // XXX(ckatsak): Optional
		NetworkInterfaces: []*proto.FirecrackerNetworkInterface{{
			StaticConfig: &proto.StaticNetworkConfiguration{
				MacAddress:  "AA:FC:00:00:05:01",
				HostDevName: "ckatsak.tap.01",
				IPConfig: &proto.IPConfiguration{
					PrimaryAddr: "10.0.1.2/24",
					GatewayAddr: "10.0.1.1",
					Nameservers: []string{"1.1.1.1", "1.0.0.1"},
				},
			},
		}},
		ContainerCount:           1,
		ExitAfterAllTasksDeleted: true,
	}
	resp, err := c.fcc.CreateVM(ctx, req)
	if err != nil {
		log.WithError(err).Errorf("error creating new VM")
		return
	}
	defer func() {
		log.Infof("Stopping uVM...")
		if _, err := c.fcc.StopVM(ctx, &proto.StopVMRequest{VMID: vmID}); err != nil {
			log.WithError(err).Warnf("failed to StopVM()")
		}
	}()
	log.Debugf("fcClient.CreateVM() responded: %#v", resp)
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
			//oci.WithTTY,
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
			log.WithError(err).Warnf("failed to delete container '%s' with snapshot-cleanup", container.ID())
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a new task from the container we just defined, to run inside the uVM
	log.Infof("Creating the new task...")
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio /*, cio.WithTerminal */))
	if err != nil {
		log.WithError(err).Errorf("failed to create new task")
		return
	}
	defer func() {
		log.Infof("Deleting task...")
		exitStatus, err := task.Delete(ctx)
		if err != nil {
			log.WithError(err).Warnf("failed to delete task")
		}
		log.Infof("Deleted task's exit status: %#v", exitStatus)
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a channel to wait for the task to complete before exiting
	log.Infof("Creating the new task's exit channel...")
	esCh, err := task.Wait(ctx)
	if err != nil {
		log.WithError(err).Errorf("failed to (*containerd.Task).Wait()")
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Start the task
	log.Infof("Starting the new task...")
	if err = task.Start(ctx); err != nil {
		log.WithError(err).Errorf("failed to start the new Task")
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Wait for the task to complete before exiting
	log.Infof("Waiting for the task to complete...")
	es := <-esCh
	log.Infof("Task completed with exit status: %#v", es)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
}

func createSnapshot() {
	var err error

	log := log.WithField("func", "createSnapshot()")
	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)

	c, err := NewClient(defaultContainerdAddress, defaultContainerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("failed to create client")
		return
	}

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
	// Create new uVM
	log.Infof("Creating new uVM...")
	createReq := &proto.CreateVMRequest{
		VMID:           vmID,
		TimeoutSeconds: 20, // XXX(ckatsak): Optional
		NetworkInterfaces: []*proto.FirecrackerNetworkInterface{{
			StaticConfig: &proto.StaticNetworkConfiguration{
				MacAddress:  "AA:FC:00:00:05:01",
				HostDevName: "ckatsak.tap.01",
				IPConfig: &proto.IPConfiguration{
					PrimaryAddr: "10.0.1.2/24",
					GatewayAddr: "10.0.1.1",
					Nameservers: []string{"1.1.1.1", "1.0.0.1"},
				},
			},
		}},
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
		if _, err := c.fcc.StopVM(ctx, &proto.StopVMRequest{VMID: vmID}); err != nil {
			log.WithError(err).Warnf("failed to StopVM() on deferred call")
		}
	}()
	log.Debugf("fcClient.CreateVM() responded: %#v", resp)
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

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a new task from the container we just defined, to run inside the uVM
	log.Infof("Creating the new task...")
	var task containerd.Task
	task, err = container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
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

	if err = os.MkdirAll(defaultSnapshotDirname, 0775); err != nil {
		log.WithError(err).Errorf("failed to mkdir('%s') for snapshots", defaultSnapshotDirname)
		return
	}
	log.Infof("Sleeping for 10 seconds for you to play...")
	time.Sleep(10 * time.Second)

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
	log.Infof("uVM snapshots have been created successfully!")
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	// Return, allowing the deferred calls to attempt to resume the uVM and cleanup the task and the container
}

func main() {
	switch len(os.Args) {
	case 2:
		switch os.Args[1] {
		case "snap":
			createSnapshot()
		case "vanilla":
			vanilla()
		}
	default:
		vanilla()
	}
}
