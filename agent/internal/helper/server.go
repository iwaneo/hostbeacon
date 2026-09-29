package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/config"
)

const (
	maxRequest     = 4096
	requestTimeout = 10 * time.Second
	jobTimeout     = 5 * time.Minute
	// maxConnections limits what a faulty network part can start at once.
	maxConnections = 8
)

// Jobs does the work of each job.
type Jobs interface {
	ReadSmart(ctx context.Context) ([]SmartDisk, error)
	ReadContainers(ctx context.Context) (Containers, error)
	// WatchContainers calls changed after each container event until ctx
	// ends or the event stream fails.
	WatchContainers(ctx context.Context, changed func()) error
	ReadSMBIOSUUID(ctx context.Context) (*string, error)
}

// Server is the root helper.
type Server struct {
	// AllowedUID is the Agent user. Every other caller is refused, root too.
	AllowedUID int
	// LoadConfig reads the Host config. It runs for every request; when it
	// fails, the request is refused.
	LoadConfig func() (config.Config, error)
	Jobs       Jobs
	// Actions checks, logs, and runs Actions. Without it, every Action
	// request is an error.
	Actions *ActionRunner
	Log     *slog.Logger
}

// Serve answers connections until ctx ends.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	slots := make(chan struct{}, maxConnections)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			// Too many requests at once. Nothing is written, since the
			// caller is not checked yet.
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer conn.Close()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	if uid, err := peerUID(unixConn); err != nil || uid != s.AllowedUID {
		s.Log.Warn("refused a caller that is not the Agent user", "uid", uid, "error", err)
		writeReply(conn, reply{Error: "not allowed: only the Agent user may use the helper"})
		return
	}
	conn.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequest)).ReadBytes('\n')
	if err != nil {
		writeReply(conn, reply{Error: "cannot read the request"})
		return
	}
	conn.SetReadDeadline(time.Time{})
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		writeReply(conn, reply{Error: "cannot read the request"})
		return
	}
	// Fail closed: without a readable Host config, no job runs.
	cfg, err := s.LoadConfig()
	if err != nil {
		s.Log.Error("refused a request: cannot read the Host config", "job", req.Job, "error", err)
		writeReply(conn, reply{Error: fmt.Sprintf("refused: cannot read the Host config: %v", err)})
		return
	}
	switch req.Job {
	case JobReadSmart:
		s.answer(ctx, conn, func(ctx context.Context) (any, error) { return s.Jobs.ReadSmart(ctx) })
	case JobReadContainers:
		s.answer(ctx, conn, func(ctx context.Context) (any, error) { return s.Jobs.ReadContainers(ctx) })
	case JobReadSMBIOSUUID:
		s.answer(ctx, conn, func(ctx context.Context) (any, error) { return s.Jobs.ReadSMBIOSUUID(ctx) })
	case JobWatchContainers:
		s.watch(ctx, conn)
	case JobAction:
		s.act(ctx, conn, cfg, req.Action)
	case JobLogIdentityCopy:
		s.logIdentityCopy(conn, req.Copy)
	default:
		s.Log.Warn("refused an unknown job", "job", req.Job)
		writeReply(conn, reply{Error: fmt.Sprintf("unknown job %q", req.Job)})
	}
}

func (s *Server) answer(ctx context.Context, conn net.Conn, job func(context.Context) (any, error)) {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	result, err := job(ctx)
	if err != nil {
		writeReply(conn, reply{Error: err.Error()})
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		writeReply(conn, reply{Error: err.Error()})
		return
	}
	writeReply(conn, reply{Result: data})
}

// act answers an Action request with the Ack, and for an accepted Action,
// runs it and answers its result.
func (s *Server) act(ctx context.Context, conn net.Conn, cfg config.Config, request *ActionRequest) {
	if s.Actions == nil || request == nil {
		writeReply(conn, reply{Error: "no Action in the request"})
		return
	}
	ack, run, err := s.Actions.Request(cfg, *request)
	if err != nil {
		writeReply(conn, reply{Error: err.Error()})
		return
	}
	data, _ := json.Marshal(ack)
	// The Action runs even when the caller has left.
	writeReply(conn, reply{Result: data})
	if run == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	data, _ = json.Marshal(run(ctx))
	writeReply(conn, reply{Result: data})
}

// logIdentityCopy writes the network part's report that it is a copy to the
// Action log. The helper cannot check the report; it only keeps it.
func (s *Server) logIdentityCopy(conn net.Conn, copy *IdentityCopy) {
	if s.Actions == nil || copy == nil {
		writeReply(conn, reply{Error: "no identity copy in the request"})
		return
	}
	if err := s.Actions.Log.LogIdentityCopy(*copy); err != nil {
		writeReply(conn, reply{Error: err.Error()})
		return
	}
	s.Log.Warn("the Agent found that this Host is a copy and made a new identity", "instance_id", copy.InstanceID, "copied_from", copy.CopiedFrom)
	if err := s.Actions.ResetUpdateRun(); err != nil {
		s.Log.Error("cannot reset the Update run record of the copy", "error", err)
	}
	writeReply(conn, reply{Result: json.RawMessage("{}")})
}

// watch sends one line per container change. It stops when the caller
// closes the connection.
func (s *Server) watch(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// The caller sends nothing more; a read returns when it leaves.
		io.Copy(io.Discard, conn)
		cancel()
	}()
	var mu sync.Mutex
	err := s.Jobs.WatchContainers(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if writeReply(conn, reply{Result: json.RawMessage("{}")}) != nil {
			cancel()
		}
	})
	if err != nil && ctx.Err() == nil {
		mu.Lock()
		defer mu.Unlock()
		writeReply(conn, reply{Error: err.Error()})
	}
}

func writeReply(conn net.Conn, r reply) error {
	conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	data, _ := json.Marshal(r)
	_, err := conn.Write(append(data, '\n'))
	return err
}
