package ansible

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"

	"github.com/mitchellh/packer/packer"
	"golang.org/x/crypto/ssh"
)

type communicatorProxy struct {
	done   <-chan struct{}
	l      net.Listener
	config *ssh.ServerConfig
	ui     packer.Ui
	comm   packer.Communicator
}

func newCommunicatorProxy(done <-chan struct{}, l net.Listener, config *ssh.ServerConfig, ui packer.Ui, comm packer.Communicator) *communicatorProxy {
	return &communicatorProxy{
		done:   done,
		l:      l,
		config: config,
		ui:     ui,
		comm:   comm,
	}
}

func (c *communicatorProxy) Serve() {
	c.ui.Say(fmt.Sprintf("SSH proxy: serving on %s", c.l.Addr()))

	errc := make(chan error, 1)

	go func(errc chan error) {
		for err := range errc {
			if err != nil {
				c.ui.Error(err.Error())
			}
		}
	}(errc)

	for {
		// Accept will return if either the underlying connection is closed or if a connection is made.
		// after returning, check to see if c.done can be received. If so, then Accept() returned because
		// the connection has been closed.
		conn, err := c.l.Accept()
		select {
		case <-c.done:
			return
		default:
			if err != nil {
				c.ui.Error(fmt.Sprintf("listen.Accept failed: %v", err))
			}
			go func(conn net.Conn) {
				errc <- c.Handle(conn, errc)
			}(conn)
		}
	}

	close(errc)
}

func (c *communicatorProxy) Handle(conn net.Conn, errc chan<- error) error {
	c.ui.Say("SSH proxy: accepted connection")
	_, chans, reqs, err := ssh.NewServerConn(conn, c.config)
	if err != nil {
		return errors.New("failed to handshake")
	}

	// discard all global requests
	go ssh.DiscardRequests(reqs)

	// Service the incoming NewChannels
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		go func(errc chan<- error) {
			errc <- c.handleSession(newChannel)
		}(errc)
	}

	return nil
}

func (c *communicatorProxy) handleSession(newChannel ssh.NewChannel) error {
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return err
	}
	defer channel.Close()

	done := make(chan struct{})

	// Sessions have requests such as "pty-req", "shell", "env", and "exec".
	// see RFC 4254, section 6
	go func(in <-chan *ssh.Request) {
		env := make([]envRequestData, 4)
		for req := range in {
			switch req.Type {
			case "env":
				req.Reply(true, nil)

				data := new(envRequestData)
				err := ssh.Unmarshal(req.Payload, data)
				if err != nil {
					c.ui.Error(err.Error())
					continue
				}
				env = append(env, *data)
			case "exec":
				req.Reply(true, nil)

				if len(req.Payload) > 0 {
					cmd := &packer.RemoteCmd{
						Stdin:   channel,
						Stdout:  channel,
						Stderr:  channel.Stderr(),
						Command: string(req.Payload),
					}
					if err := cmd.StartWithUi(c.comm, c.ui); err != nil {
						c.ui.Error(err.Error())
						close(done)
						return
					}

					exitStatus := make([]byte, 4)
					binary.BigEndian.PutUint32(exitStatus, uint32(cmd.ExitStatus))
					channel.SendRequest("exit-status", false, exitStatus)
				}
				close(done)
			default:
				c.ui.Say(fmt.Sprintf("rejecting %s request", req.Type))
				req.Reply(false, nil)
			}
		}
	}(requests)

	<-done
	return nil
}

func (c *communicatorProxy) Shutdown() {
	c.l.Close()
}

type envRequestData struct {
	Name  string
	Value string
}
