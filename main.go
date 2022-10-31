package main

import (
	"context"
	"fmt"

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

// getImage returns the containerd.Image associated with the provided image name.
//
// Also see:
// - https://github.com/estesp/examplectr/blob/master/examplectr.go
func getImage(ctx context.Context, c *containerd.Client, imageName string) (image containerd.Image, err error) {
	log := log.WithField("func", "getImage()")

	if image, err = c.GetImage(ctx, imageName); err == nil {
		log.Infof("Image found through (*containerd.Client).GetImage()")
		return
	}

	// if the image isn't already in our namespaced context, then pull it
	log.WithError(err).Infof("Pulling image")
	if image, err = c.Pull(
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

func main() {
	log := log.WithField("func", "main()")

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create containerd client
	log.Infof("Creating new containerd client...")
	client, err := containerd.New(defaultContainerdAddress)
	if err != nil {
		log.WithError(err).Errorf("error creating containerd client")
		return
	}
	defer func() {
		log.Infof("Closing containerd client...")
		if err := client.Close(); err != nil {
			log.WithError(err).Warnf("error closing containerd client")
		}
	}()

	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Version info for containerd.Client
	if v, err := client.Version(ctx); err != nil {
		log.WithError(err).Errorf("failed to get containerd's version information")
		return
	} else {
		log.Infof("(*containerd.Client).Version() returned %#v", v)
	}
	log.Infof("(*containerd.Client).Runtime() returned '%s'", client.Runtime())
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Random containerd.Client test... FIXME(ckatsak): to be removed
	log.Debugf("Random testing for containerd client...")
	cst, err := client.ContentStore().ListStatuses(ctx)
	if err != nil {
		log.WithError(err).Errorf("ContentStore().ListStatuses() failed")
		return
	}
	log.Debugf("%#v", cst)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Bit more relevant containerd.Client test... FIXME(ckatsak): to be removed
	log.Debugf("Bit-more-relevant-but-still-random testing for containerd client...")
	images, err := client.ImageService().List(ctx)
	if err != nil {
		log.WithError(err).Errorf("client.ImageService().List() failed")
		return
	}
	log.Debugf("images = %#v", images)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create firecracker-containerd fc-control client
	log.Infof("Creating new firecracker-containerd client...")
	fcClient, err := fcclient.New(defaultContainerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("fcclient.New() failed")
		return
	}
	defer func() {
		log.Infof("Closing firecracker-containerd client...")
		if err := fcClient.Close(); err != nil {
			log.WithError(err).Warnf("error closing firecracker-control client")
		}
	}()
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
	resp, err := fcClient.CreateVM(ctx, req)
	if err != nil {
		log.WithError(err).Errorf("error creating new VM")
		return
	}
	defer func() {
		log.Infof("Stopping uVM...")
		if _, err := fcClient.StopVM(ctx, &proto.StopVMRequest{VMID: vmID}); err != nil {
			log.WithError(err).Warnf("failed to StopVM()")
		}
	}()
	log.Debugf("fcClient.CreateVM() responded: %#v", resp)
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Fetch the container image to be deployed inside the uVM
	log.Infof("Fetching the container image...")
	image, err := getImage(ctx, client, imageName)
	if err != nil {
		log.WithError(err).Errorf("failed to getImage()")
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create the new container to run inside the uVM
	log.Infof("Creating the new container...")
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
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
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
