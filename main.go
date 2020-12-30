package main

import (
	"context"
	"flag"
	"os"
	"os/signal"

	"github.com/docker/docker/client"
)

var (
	flagReplicas int
	flagImage    string
)

func main() {
	flag.IntVar(&flagReplicas, "replicas", 2, "number of replicas")
	flag.StringVar(&flagImage, "image", "tigres/minecraft-fabric:latest", "docker image for servers")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	cli, err := client.NewClientWithOpts(client.FromEnv,
		client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	s, err := NewSession(cli, flagImage, flagReplicas)
	if err != nil {
		panic(err)
	}
	s.Init(ctx)
	s.Loop(ctx)
}
