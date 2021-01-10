package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var (
	logExpression = regexp.MustCompile(`^\[(\d+:\d+:\d+)\] \[([\s\w/-]+)\]: (.+)$`)
)

type Game struct {
	ID     int
	Name   string
	Image  string
	Addr   string
	Ready  bool
	Events chan Event

	Client  *client.Client
}

// Command attaches to the container and sends a command.
func (g *Game) Command(ctx context.Context, command string) error {
	resp, err := g.Client.ContainerAttach(ctx, g.Name, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
	})
	if err != nil {
		return err
	}
	defer resp.Close()

	fmt.Fprintf(resp.Conn, "%s\n", command)
	return nil
}

// Say uses the /tellraw command to send a message to all players.
func (g *Game) Say(ctx context.Context, text string, color string) error {
	buf, _ := json.Marshal([]Message{
		{Text: text, Color: color},
	})
	return g.Command(ctx, fmt.Sprintf("/tellraw @a %s", buf))
}

// Launch keeps the container alive. Each time the container is removed,
// this function starts the container again.
func (g *Game) Launch(ctx context.Context) {
	for {
		okchan, errchan := g.Client.ContainerWait(ctx, g.Name, container.WaitConditionRemoved)
		select {
		case <-okchan:
			log.Printf("[%s], removed container", g.Name)
		case err := <-errchan:
			log.Printf("[%s] error waiting for container: %s", g.Name, err)
		case <-ctx.Done():
			return
		}
		err := g.Start(ctx)
		if err != nil {
			log.Printf("[%s] error starting container: %s", g.Name, err)
		}
	}
}

// Start creates and starts a container.
func (g *Game) Start(ctx context.Context) error {
	resp, err := g.Client.ContainerCreate(ctx, &container.Config{
		Image:     g.Image,
		User:      "1337:1337",
		Tty:       true,
		OpenStdin: true,
	}, &container.HostConfig{
		AutoRemove: true,
	}, nil, nil, g.Name)
	if err != nil {
		return err
	}
	err = g.Client.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
	}
	log.Printf("[%s] started container", g.Name)
	return nil
}

// Refresh inspects the container and updates the IP address.
func (g *Game) Refresh(ctx context.Context) error {
	c, err := g.Client.ContainerInspect(ctx, g.Name)
	if err != nil {
		return err
	}
	g.Addr = c.NetworkSettings.DefaultNetworkSettings.IPAddress
	return nil
}

// HandleLog parses container log lines and generates game events.
func (g *Game) HandleLog(line string) {
	log.Printf("[%s] %s", g.Name, line)
	m := logExpression.FindAllStringSubmatch(line, 1)
	if len(m) != 1 {
		return
	}
	if len(m[0]) != 4 {
		return
	}
	ts, text := m[0][1], m[0][3]

	t, err := time.Parse("15:04:05", ts)
	if err != nil {
		log.Printf("[%s] error parsing time: %s", g.Name, err)
		return
	}
	now := time.Now()
	t = time.Date(now.Year(), now.Month(), now.Day(),
		t.Hour(), t.Minute(), t.Second(),
		now.Nanosecond(), time.UTC)

	var typ string
	switch {
	case strings.Contains(text, "> rr"):
		typ = "cmd.reset"
	case strings.Contains(text, ": Set the time to 0]"):
		typ = "cmd.retime"
	case strings.Contains(text, "For help, type \"help\""):
		typ = "generated"
	case strings.Contains(text, "joined the game"):
		typ = "login"
	case strings.Contains(text, "[We Need to Go Deeper]"):
		typ = "nether"
	case strings.Contains(text, "[The End?]"):
		typ = "end"
	case strings.Contains(text, "[Credits!]"):
		typ = "credits"
	}

	if typ != "" {
		g.Events <- Event{
			Timestamp: t,
			GameID:    g.ID,
			Type:      typ,
			Payload:   text,
		}
	}
}

// Monitor watches container logs and passes new lines to HandleLog().
func (g *Game) Monitor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			r, err := g.Client.ContainerLogs(ctx, g.Name, types.ContainerLogsOptions{
				ShowStdout: true,
				Follow:     true,
			})
			if err != nil {
				log.Printf("[%s] error monitoring logs: %s", g.Name, err)
				time.Sleep(time.Second)
				continue
			}

			rd := bufio.NewReader(r)
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					log.Printf("[%s] error reading logs: %s", g.Name, err)
					time.Sleep(time.Second)
					break
				}
				line = strings.Trim(line, "\r\n")

				g.HandleLog(line)
			}
		}
	}
}

// Reset marks a server as not-ready and kills the container.
func (g *Game) Reset(ctx context.Context) error {
	g.Ready = false
	err := g.Client.ContainerKill(ctx, g.Name, "KILL")
	if err != nil {
		return err
	}
	return nil
}
