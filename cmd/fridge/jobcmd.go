package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"

	"fridge/internal/job"
)

// runJob is `fridge job <run|rmvolume>`: what swarm job containers execute. Never used by people.
//
//	run               do the job described by $FRIDGE_JOB (dump, restore or check a backup)
//	pull <image>      pull an image on the host the job is pinned to (needs the Docker socket)
//	rmvolume <name>   remove a local Docker volume of the host the job is pinned to (needs the Docker socket)
func runJob(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) == 0 {
		fmt.Fprintln(stderr, "job: missing subcommand (run | pull | rmvolume)")
		return 2
	}
	switch args[0] {
	case "run":
		s, err := job.FromEnv()
		if err != nil {
			fmt.Fprintln(stderr, "job:", err)
			return 2
		}
		return job.Run(ctx, s, stdout, stderr)
	case "pull":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "job pull: want one image reference")
			return 2
		}
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			fmt.Fprintln(stderr, "job:", err)
			return 1
		}
		defer cli.Close()
		rc, err := cli.ImagePull(ctx, args[1], image.PullOptions{})
		if err == nil {
			_, err = io.Copy(io.Discard, rc)
			rc.Close()
		}
		if err != nil {
			fmt.Fprintln(stderr, "job pull:", err)
			return 1
		}
		return 0
	case "rmvolume":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "job rmvolume: want one volume name")
			return 2
		}
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			fmt.Fprintln(stderr, "job:", err)
			return 1
		}
		defer cli.Close()
		// the container of the removed service goes a moment after its task does: "in use" passes
		for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(time.Second) {
			err = cli.VolumeRemove(ctx, args[1], true)
			if err == nil || errdefs.IsNotFound(err) || !strings.Contains(err.Error(), "in use") || time.Now().After(deadline) {
				break
			}
		}
		if err != nil && !errdefs.IsNotFound(err) {
			fmt.Fprintln(stderr, "job rmvolume:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "job: unknown subcommand %q\n", args[0])
	return 2
}
