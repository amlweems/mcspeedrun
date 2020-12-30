package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/dantoye/throwpro/throwlib"
	"github.com/docker/docker/client"
)

const (
	StateFile = "state.json"
)

type Message struct {
	Text  string `json:"text"`
	Color string `json:"color"`
}

type Event struct {
	GameID    int
	Timestamp time.Time
	Type      string
	Payload   string
}

type SessionData struct {
	Attempt int `json:"attempt"`
}

type Session struct {
	Events chan Event
	Client *client.Client
	Data   SessionData

	Replicas map[int]*Game
	Image    string

	Active    *Game
	State     string
	TimeStart time.Time

	ProxyAddr chan string
}

// NewSession creates a session, loads state, and initializes the replicas.
func NewSession(cli *client.Client, image string, replicas int) (*Session, error) {
	s := &Session{
		Client:    cli,
		Image:     image,
		Replicas:  make(map[int]*Game),
		Events:    make(chan Event),
		ProxyAddr: make(chan string),
	}
	err := s.Load()
	if err != nil {
		return nil, err
	}
	for i := 0; i < replicas; i++ {
		s.NewGame(i)
	}
	return s, nil
}

// NewGame creates a new game object and adds it to the session.
func (s *Session) NewGame(id int) {
	s.Replicas[id] = &Game{
		ID:      id,
		Image:   s.Image,
		Name:    fmt.Sprintf("mcspeedrun_%d", id),
		Thrower: throwlib.NewSession(),
		Client:  s.Client,
		Events:  s.Events,
	}
}

// Init launches the Launch() and Monitor() goroutines in each replica.
// It also starts the Proxy() goroutine on the Session.
func (s *Session) Init(ctx context.Context) {
	for _, replica := range s.Replicas {
		go replica.Launch(ctx)
		go replica.Monitor(ctx)
	}
	go s.Proxy(ctx)
}

// Loop monitors game events and updates the internal state machine.
// Some events interact with the active game (e.g. to broadcast a
// message to all players).
func (s *Session) Loop(ctx context.Context) {
	for {
		// if we're missing an active game, attempt to find one
		if s.Active == nil {
			s.ProxyAddr <- ""
			for _, replica := range s.Replicas {
				if replica.Ready {
					log.Printf("[core] switching to %s", replica.Name)
					s.Active = replica
					s.ProxyAddr <- s.Active.Addr
					break
				}
			}
		}

		select {
		case <-ctx.Done():
			log.Printf("[core] shutting down")
			err := s.Save()
			if err != nil {
				log.Printf("[core] error saving attempt: %s", err)
			}
			return
		case evt := <-s.Events:
			log.Printf("[core] received '%s' from %d", evt.Type, evt.GameID)

			// skip events with invalid game IDs
			if evt.GameID >= len(s.Replicas) {
				log.Printf("[core] unknown game ID %d", evt.GameID)
				continue
			}

			// skip all events with mismatched IDs except world gen events
			if (s.Active == nil || evt.GameID != s.Active.ID) && evt.Type != "generated" {
				log.Printf("[core] %s event from non-active game %d", evt.Type, evt.GameID)
				continue
			}

			switch evt.Type {
			case "cmd.reset":
				s.State = ""
				s.Data.Attempt += 1
				s.Active.Reset(ctx)
				s.Active = nil

			case "cmd.player":
				s.Active.HandleThrow(ctx, evt.Payload)

			case "cmd.pearl":
				s.Active.HandleThrow(ctx, evt.Payload)
				text := fmt.Sprintf("Pearl: [%s]", evt.Timestamp.Sub(s.TimeStart))
				s.Active.Say(ctx, text, "green")

			case "generated":
				s.Replicas[evt.GameID].Ready = true
				s.Replicas[evt.GameID].Refresh(ctx)
				log.Printf("[core] server %d is online", evt.GameID)

			case "login":
				if s.State != "" {
					continue
				}
				s.State = "overworld"
				s.TimeStart = evt.Timestamp
				s.Active.Say(ctx, fmt.Sprintf("attempt #%d", s.Data.Attempt), "green")
				s.Active.Command(ctx, "/time set 0")
				s.Active.Command(ctx, "/save-off")

			case "nether":
				if s.State != "overworld" {
					continue
				}
				s.State = "nether"
				text := fmt.Sprintf("Nether: [%s]", evt.Timestamp.Sub(s.TimeStart))
				s.Active.Say(ctx, text, "green")

			case "end":
				if s.State != "nether" {
					continue
				}
				s.State = "end"
				text := fmt.Sprintf("End: [%s]", evt.Timestamp.Sub(s.TimeStart))
				s.Active.Say(ctx, text, "green")

			case "credits":
				if s.State != "end" {
					continue
				}
				s.State = "credits"
				text := fmt.Sprintf("Credits: [%s]", evt.Timestamp.Sub(s.TimeStart))
				s.Active.Say(ctx, text, "green")
			}
		}
	}
}

// Load loads all SessionData from the state.json file.
func (s *Session) Load() error {
	f, err := os.Open(StateFile)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}
	err = json.NewDecoder(f).Decode(&s.Data)
	if err != nil {
		return err
	}
	return err
}

// Save saves all SessionData to the state.json file.
func (s *Session) Save() error {
	f, err := os.OpenFile(StateFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(s.Data)
	if err != nil {
		return err
	}
	return f.Close()
}

// Proxy listens on the standard Minecraft port and proxies all traffic to the
// active replica. The replica address is updated via the ProxyAddr channel.
func (s *Session) Proxy(ctx context.Context) {
	proxyAddr := <-s.ProxyAddr

	for {
		l, err := net.Listen("tcp", "0.0.0.0:25565")
		if err != nil {
			panic(err)
		}

		go func(l net.Listener) {
			select {
			case proxyAddr = <-s.ProxyAddr:
				log.Printf("[proxy] switching to %s", proxyAddr)
			case <-ctx.Done():
			}
			l.Close()
		}(l)

		for {
			conn, err := l.Accept()
			if err != nil {
				log.Printf("[proxy] error accepting connection: %s", err)
				break
			}
			if proxyAddr == "" {
				conn.Close()
				continue
			}
			log.Printf("%s -> %s", conn.RemoteAddr(), proxyAddr)

			// Handle the connection in a new goroutine.
			go func(c net.Conn) {
				var proxy net.Conn
				var err error

				// connect to proxy address
				proxy, err = net.Dial("tcp", proxyAddr+":25565")
				if err != nil {
					log.Printf("[proxy] error connecting to proxy: %s", err)
					c.Close()
					return
				}

				// Close the connection once.
				var once sync.Once
				onceBody := func() {
					c.Close()
					proxy.Close()
				}

				// Read from conn, send to proxy.
				go func(c net.Conn) {
					io.Copy(proxy, c)
					once.Do(onceBody)
				}(c)

				// Read from proxy, send to conn.
				go func(c net.Conn) {
					io.Copy(c, proxy)
					once.Do(onceBody)
				}(c)
			}(conn)
		}

		l.Close()
	}
}
