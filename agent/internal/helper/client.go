package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"
)

const (
	callTimeout = 6 * time.Minute
	maxReply    = 4 << 20
)

// Client is the network part's side of the helper socket.
type Client struct {
	Socket string
}

// ReadSmart reads SMART from every physical disk. A disk in standby is
// listed but not read.
func (c Client) ReadSmart(ctx context.Context) ([]SmartDisk, error) {
	var disks []SmartDisk
	return disks, c.call(ctx, JobReadSmart, &disks)
}

// ReadContainers lists every Docker and Podman container.
func (c Client) ReadContainers(ctx context.Context) (Containers, error) {
	var containers Containers
	return containers, c.call(ctx, JobReadContainers, &containers)
}

// ReadSMBIOSUUID reads the SMBIOS UUID; nil when the Host has none.
func (c Client) ReadSMBIOSUUID(ctx context.Context) (*string, error) {
	var uuid *string
	return uuid, c.call(ctx, JobReadSMBIOSUUID, &uuid)
}

// WatchContainers calls changed after each container event, until ctx ends
// or the helper stops the watch.
func (c Client) WatchContainers(ctx context.Context, changed func()) error {
	conn, err := c.send(ctx, JobWatchContainers)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	lines := bufio.NewReader(conn)
	for {
		r, err := readReply(lines)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if err := errorFromReply(r); err != nil {
			return err
		}
		changed()
	}
}

func (c Client) call(ctx context.Context, job string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	conn, err := c.send(ctx, job)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetReadDeadline(deadline)
	r, err := readReply(bufio.NewReader(conn))
	if err != nil {
		return err
	}
	if err := errorFromReply(r); err != nil {
		return err
	}
	return json.Unmarshal(r.Result, result)
}

func (c Client) send(ctx context.Context, job string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(request{Job: job})
	conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		// A refused caller may be closed before it writes; its reply says why.
		if r, readErr := readReply(bufio.NewReader(conn)); readErr == nil && r.Error != "" {
			conn.Close()
			return nil, errorFromReply(r)
		}
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func readReply(lines *bufio.Reader) (reply, error) {
	var line []byte
	for {
		part, isPrefix, err := lines.ReadLine()
		if err != nil {
			return reply{}, err
		}
		line = append(line, part...)
		if len(line) > maxReply {
			return reply{}, errors.New("helper: the reply is too long")
		}
		if !isPrefix {
			break
		}
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return reply{}, errors.New("helper: cannot read the reply")
	}
	return r, nil
}

// errorFromReply turns a reply's error text into an error.
func errorFromReply(r reply) error {
	if r.Error != "" {
		return errors.New("helper: " + r.Error)
	}
	return nil
}
