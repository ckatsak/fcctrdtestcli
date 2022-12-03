package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/snapshots"

	fcclient "github.com/firecracker-microvm/firecracker-containerd/firecracker-control/client"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/firecracker-microvm/firecracker-containerd/runtime/firecrackeroci"
)

const (
	defaultNamespace              = namespaces.Default
	defaultContainerdAddress      = "/run/firecracker-containerd/containerd.sock"
	defaultContainerdTTRPCAddress = defaultContainerdAddress + ".ttrpc"

	devmapperSnapshotter = "devmapper"

	kernelArgs = "i8042.nokbd i8042.noaux 8250.nr_uarts=0 ipv6.disable=1 noapic reboot=k panic=1 pci=off nomodules ro systemd.unified_cgroup_hierarchy=0 systemd.journald.forward_to_console systemd.unit=firecracker.target init=/sbin/overlay-init"

	imagePrefix = "docker.io/ckatsak/snaplace-fbpml-"
	imageTag    = "0.0.1-dev"

	nginxImageName = "docker.io/library/nginx@sha256:943c25b4b66b332184d5ba6bb18234273551593016c0e0ae906bab111548239f"
)

var (
	defaultNetworkInterface = &proto.FirecrackerNetworkInterface{
		StaticConfig: &proto.StaticNetworkConfiguration{
			MacAddress:  "AA:FC:00:00:05:01",
			HostDevName: "ckatsak.tap.01",
			IPConfig: &proto.IPConfiguration{
				PrimaryAddr: "10.0.1.2/24",
				GatewayAddr: "10.0.1.1",
				Nameservers: []string{"1.1.1.1", "1.0.0.1"},
			},
		},
	}

	log *logrus.Entry
)

func init() {
	log = logrus.New().WithFields(logrus.Fields{
		"namespace": defaultNamespace,
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

	log.Infof("Creating new frecracker-containerd client...")
	client, err := containerd.New(containerdAddress)
	if err != nil {
		log.WithError(err).Errorf("error creating firecracker-containerd client")
		return
	}
	defer func() {
		if err != nil {
			log.Infof("Closing containerd client...")
			if err := client.Close(); err != nil {
				log.WithError(err).Warnf("error closing firecracker-containerd client")
			}
		}
	}()

	log.Infof("Creating new fc-control client...")
	fcClient, err := fcclient.New(containerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("error creating fc-control client")
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
		containerd.WithPullSnapshotter(devmapperSnapshotter),
	); err != nil {
		err = fmt.Errorf("failed to (*containerd.Client).Pull('%s'): %v", imageName, err)
		log.WithError(err).Warn()
	}
	return
}

func (c *Client) parent(ctx context.Context, bench, fullImageRef string) (parentSnapshot string) {
	var (
		vmID    = "cli06-" + strings.ReplaceAll(bench, "_", "-")
		snapKey = vmID + "-snap"
		log     = log.WithFields(logrus.Fields{
			"mode":  "parent",
			"bench": bench,
			"image": fullImageRef,
			"vmID":  vmID,
		})
		err error
	)

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Fetch the container image to be deployed inside the uVM
	log.Infof("Fetching the container image...")
	image, err := c.getImage(ctx, fullImageRef)
	if err != nil {
		log.WithError(err).Errorf("failed to getImage()")
		return
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create new uVM
	log.Infof("Creating new uVM...")
	createReq := &proto.CreateVMRequest{
		VMID:                     vmID,
		TimeoutSeconds:           10,
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

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Create the new container to run inside the uVM
	log.Infof("Creating the new container...")
	container, err := c.cc.NewContainer(
		ctx,
		vmID,
		containerd.WithSnapshotter(devmapperSnapshotter),
		containerd.WithNewSnapshot(snapKey, image),
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
			log.WithError(err).Errorf("failed to delete container '%s' with snapshot-cleanup on deferred call", container.ID())
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

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
			log.WithError(err).Errorf("failed to delete task on deferred call")
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
		log := log.WithField("goroutine", "task.Wait()")
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
		log.Infof("Sending a SIGKILL to the task...")
		if err := task.Kill(ctx, syscall.SIGKILL); err != nil {
			log.WithError(err).Warnf("failed to send a SIGKILL to the task on deferred call")
		}
	}()
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Print out container's OCI runtime spec
	specFileName := "spec_" + bench + ".json"
	log.Infof("Writing container's OCI runtime spec in '%s'...", specFileName)
	if spec, err := container.Spec(ctx); err != nil {
		log.WithError(err).Warnf("failed to retrieve container's OCI runtime spec")
	} else {
		if marshalledSpec, err := json.MarshalIndent(spec, "", "    "); err != nil {
			log.WithError(err).Warnf("failed to JSON-marshal container's OCI runtime spec")
		} else {
			//log.Tracef("New container's OCI runtime spec:\n%s", marshalledSpec)
			if err := os.WriteFile(specFileName, marshalledSpec, 0666); err != nil {
				log.WithError(err).Errorf("failed to write OCI runtime spec to '%s'", specFileName)
			}
		}
	}
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	snapInfo, err := c.cc.SnapshotService(devmapperSnapshotter).Stat(ctx, snapKey)
	if err != nil {
		log.WithError(err).Errorf("failed to stat snapshot '%s'", snapKey)
		return
	}
	log.Debugf("snapshot info match: %#v", snapInfo)

	return snapInfo.Parent
}

func (c *Client) play(ctx context.Context) {
	var (
		err error
		log = log.WithFields(logrus.Fields{
			"mode": "play",
		})
	)

	img, err := c.getImage(ctx, nginxImageName)
	if err != nil {
		log.WithError(err).Errorf("failed to (*Client).GetImage('%s')", nginxImageName)
		return
	}
	log.Infof("nginx image: %#v", img)

	images, err := c.cc.ImageService().List(ctx)
	if err != nil {
		log.WithError(err).Errorf("failed to list images")
		return
	}
	for _, image := range images {
		log.Infof("- image: %#v", image)
	}

	if err = c.cc.SnapshotService(devmapperSnapshotter).Walk(ctx, func(ctx context.Context, i snapshots.Info) error {
		log.Infof("--> snapshot: %#v", i)
		return nil
	}); err != nil {
		log.WithError(err).Errorf("failed to (*containerd.Client).SnapshotService('%s').Walk()", devmapperSnapshotter)
		return
	}

	statuses, err := c.cc.ContentStore().ListStatuses(ctx)
	if err != nil {
		log.WithError(err).Errorf("failed to (*containerd.Client).ContentStore().ListStatuses()")
		return
	}
	log.Infof("len(statuses) = %d", len(statuses))
	for _, status := range statuses {
		log.Infof("- status: %#v", status)
	}

	if err = c.cc.ContentStore().Walk(ctx, func(i content.Info) error {
		log.Infof("- content: %#v", i)
		return nil
	}); err != nil {
		log.WithError(err).Errorf("failed to (*containerd.Client).ContentStore().Walk()")
		return
	}

	if err = c.cc.SnapshotService(devmapperSnapshotter).Walk(ctx, func(ctx context.Context, i snapshots.Info) error {
		log.Infof("--> snapshot: %#v", i)
		return nil
	}, "name~=testclient"); err != nil {
		log.WithError(err).Errorf("failed to (*containerd.Client).SnapshotService('%s').Walk()", devmapperSnapshotter)
		return
	}

	snapshotKey := "testclient-snap"
	if sinfo, err := c.cc.SnapshotService(devmapperSnapshotter).Stat(ctx, snapshotKey); err != nil {
		log.WithError(err).Errorf("failed to stat snapshot '%s'", snapshotKey)
	} else {
		log.Infof("snapshot info match: %#v", sinfo)
	}
}

func main() {
	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)

	c, err := NewClient(defaultContainerdAddress, defaultContainerdTTRPCAddress)
	if err != nil {
		log.WithError(err).Errorf("failed to create client")
		return
	}
	defer c.Close()

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Version info for containerd
	if v, err := c.cc.Version(ctx); err != nil {
		log.WithError(err).Errorf("failed to get containerd's version information")
		return
	} else {
		log.Infof("(*containerd.Client).Version() returned %#v", v)
	}
	log.Infof("(*containerd.Client).Runtime() returned '%s'", c.cc.Runtime())
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	///////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Uncomment these to test the `parent` method with the nginx image and then exit:
	////c.play(ctx)
	//parent := c.parent(ctx, "nginx", nginxImageName)
	//log.Infof("'%s' is the parent of '%s'", parent, nginxImageName)
	//return
	///////////////////////////////////////////////////////////////////////////////////////////////////////////////

	parents := make(map[string]string)
	for _, bench := range []string{
		"helloworld",
		"pyaes",
		"lr_serving",
		"chameleon",
		"cnn_serving",
		"rnn_serving",
		"image_rotate",
		"json_serdes",
		"matmul_fb",
		"video_processing",
		"lr_training",
	} {
		imageRef := fmt.Sprintf("%s%s:%s", imagePrefix, bench, imageTag)
		parents[imageRef] = c.parent(ctx, bench, imageRef)
	}
	if marshalledParents, err := json.MarshalIndent(parents, "", "  "); err != nil {
		log.WithError(err).Errorf("failed to JSON-marshal this: %#v", parents)
	} else {
		log.Infof("JSON-marshalled parents:\n%s", string(marshalledParents))
		if err := os.WriteFile("parents.json", marshalledParents, 0666); err != nil {
			log.WithError(err).Errorf("failed to write parents to file")
		}
	}
}
