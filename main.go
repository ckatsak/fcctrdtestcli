package main

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"

	//
	//fcctrd "github.com/ckatsak/fc-ctrd"
	//_ "github.com/ckatsak/fc-ctrd"
	//_ "github.com/firecracker-microvm/firecracker-containerd"
	//_ "github.com/firecracker-microvm/firecracker-containerd"
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
)

var logger *logrus.Logger

func init() {
	logger = logrus.New() //.WithFields(logrus.Fields{ /* TODO */ })
	logger.SetLevel(logrus.TraceLevel)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:          true,
		DisableLevelTruncation: true,
	})
}

// getImage returns the containerd.Image associated with the provided image name.
//
// Also see:
// - https://github.com/estesp/examplectr/blob/master/examplectr.go
func getImage(ctx context.Context, c *containerd.Client, imageName string) (image containerd.Image, err error) {
	if image, err = c.GetImage(ctx, imageName); err == nil {
		logger.Infof("Image '%s' found through (*containerd.Client).GetImage()", imageName)
		return
	}

	// if the image isn't already in our namespaced context, then pull it
	logger.Infof("Pulling image '%s'", imageName)
	if image, err = c.Pull(
		ctx,
		imageName,
		containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(snapshotter),
	); err != nil {
		err = fmt.Errorf("failed to (*containerd.Client).Pull('%s'): %v", imageName, err)
	}
	return
}

func main() {
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create containerd client
	logger.Infof("Creating new containerd client...")
	client, err := containerd.New(defaultContainerdAddress)
	if err != nil {
		logger.Errorf("error creating containerd client: %v", err)
		return
	}
	defer func() {
		logger.Infof("Closing containerd client...")
		if err := client.Close(); err != nil {
			logger.Warnf("error closing containerd client: %v", err)
		}
	}()

	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Version info for containerd.Client
	if v, err := client.Version(ctx); err != nil {
		logger.Errorf("failed to get containerd's version information: %v", err)
		return
	} else {
		logger.Infof("(*containerd.Client).Version() returned %#v", v)
	}
	logger.Infof("(*containerd.Client).Runtime() returned '%s'", client.Runtime())
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Random containerd.Client test... FIXME(ckatsak): to be removed
	logger.Debugf("Random testing for containerd client...")
	cst, err := client.ContentStore().ListStatuses(ctx)
	if err != nil {
		logger.Errorf("ContentStore().ListStatuses() failed: %v", err)
		return
	}
	logger.Debugf("%#v", cst)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Bit more relevant containerd.Client test... FIXME(ckatsak): to be removed
	logger.Debugf("Bit-more-relevant-but-still-random testing for containerd client...")
	images, err := client.ImageService().List(ctx)
	if err != nil {
		logger.Errorf("client.ImageService().List() failed: %v", err)
		return
	}
	logger.Debugf("images = %#v", images)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create firecracker-containerd fc-control client
	logger.Infof("Creating new firecracker-containerd client...")
	fcClient, err := fcclient.New(defaultContainerdTTRPCAddress)
	if err != nil {
		logger.Errorf("fcclient.New() failed: %v", err)
		return
	}
	defer func() {
		logger.Infof("Closing firecracker-containerd client...")
		if err := fcClient.Close(); err != nil {
			logger.Warnf("error closing firecracker-control client: %v", err)
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create new uVM
	logger.Infof("Creating new uVM (VMID: %s)...", vmID)
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
	resp, err := fcClient.CreateVM(ctx, req)
	if err != nil {
		logger.Errorf("error creating new VM: %v", err)
		return
	}
	//_ = resp
	defer func() {
		logger.Infof("Stopping uVM '%s'...", vmID)
		if _, err := fcClient.StopVM(ctx, &proto.StopVMRequest{VMID: vmID}); err != nil {
			logger.Warnf("failed to StopVM('%s'): %v", vmID, err)
		}
	}()
	logger.Debugf("fcClient.CreateVM() responded: %#v", resp)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Fetch the container image to be deployed inside the uVM
	logger.Infof("Fetching the container image...")
	image, err := getImage(ctx, client, imageName)
	if err != nil {
		logger.Errorf("failed to getImage(): %v", err)
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create the new container to run inside the uVM
	logger.Infof("Creating the new container...")
	container, err := client.NewContainer(
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
		logger.Errorf("failed to create NewContainer('%s'): %v", vmID, err)
		return
	}
	defer func() {
		logger.Infof("Deleting container '%s'...", container.ID())
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			logger.Warnf("failed to delete container '%s' with snapshot-cleanup: %v", container.ID(), err)
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a new task from the container we just defined, to run inside the uVM
	logger.Infof("Creating the new task...")
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		logger.Errorf("failed to create new task: %v", err)
		return
	}
	defer func() {
		logger.Infof("Deleting task...")
		exitStatus, err := task.Delete(ctx)
		if err != nil {
			logger.Warnf("failed to delete task: %v", err)
		}
		logger.Infof("Deleted task's exit status: %#v", exitStatus)
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create a channel to wait for the task to complete before exiting
	logger.Infof("Creating the new task's exit channel...")
	esCh, err := task.Wait(ctx)
	if err != nil {
		logger.Errorf("failed to (*containerd.Task).Wait(): %v", err)
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Start the task
	logger.Infof("Starting the new task...")
	if err = task.Start(ctx); err != nil {
		logger.Errorf("failed to start the new Task: %v", err)
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Wait for the task to complete before exiting
	logger.Infof("Waiting for the task to complete...")
	es := <-esCh
	logger.Infof("Task completed with exit status: %#v", es)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
}
