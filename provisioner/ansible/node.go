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

func (c *communicatorProxy) Serve() {
	c.ui.Say(fmt.Sprintf("SSH proxy: serving on %s", c.l.Addr()))

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
				c.ui.Say(fmt.Sprintf("listen.Accept failed: %v", err))
			}
			go c.Handle(conn)
		}
	}
}

func (c *communicatorProxy) Handle(conn net.Conn) error {
	c.ui.Say("SSH proxy: accepted connection")
	_, chans, reqs, err := ssh.NewServerConn(conn, c.config)
	if err != nil {
		return errors.New("failed to handshake")
	}

	// discard all global requests
	go ssh.DiscardRequests(reqs)

	// Service the incoming Channel channel.
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		err := c.handleSession(newChannel)
		if err != nil {
			return err
		}
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
		for req := range in {
			switch req.Type {
			case "exec":
				c.ui.Say(fmt.Sprintf("accepting %s request", req.Type))
				req.Reply(true, nil)

				if len(req.Payload) > 0 {
					cmd := &packer.RemoteCmd{
						Stdin:   channel,
						Stdout:  channel,
						Stderr:  channel.Stderr(),
						Command: string(req.Payload),
					}
					c.ui.Say(fmt.Sprintf("starting %s", cmd.Command))
					if err := c.comm.Start(cmd); err != nil {
						c.ui.Say(fmt.Sprint(err))
						close(done)
						return
					}
					cmd.Wait()

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
