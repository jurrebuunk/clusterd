// Package actions holds what clusterd may change. Every action checks its
// preconditions, takes the cluster-wide lock, streams progress and leaves an
// audit record in the journal.
package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
)

// Ceph runs the ceph CLI with the configured key and decodes its JSON.
func (e *Engine) ceph(ctx context.Context, out any, args ...string) error {
	cc := e.Cfg.Ceph
	if cc.Keyring == "" || cc.ID == "" {
		return errors.New("ceph is not configured")
	}
	full := append([]string{"--id", cc.ID, "--keyring", cc.Keyring, "--connect-timeout", "10"}, args...)
	if out != nil {
		full = append(full, "--format", "json")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := e.Run(ctx, cc.Command, full...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// Kube is the part of the Kubernetes API actions use (a fake in tests).
type Kube interface {
	Nodes(ctx context.Context) ([]api.K3sNode, error)
	Node(ctx context.Context, name string) (api.K3sNode, error)
	SetUnschedulable(ctx context.Context, name string, v bool) error
	PodsOnNode(ctx context.Context, node string) ([]kube.Pod, error)
	Evict(ctx context.Context, p kube.Pod) error
	PodGone(ctx context.Context, p kube.Pod) (bool, error)
	Readyz(ctx context.Context) error
	GetLease(ctx context.Context, ns, name string) (*kube.Lease, error)
	CreateLease(ctx context.Context, l *kube.Lease) (*kube.Lease, error)
	UpdateLease(ctx context.Context, l *kube.Lease) (*kube.Lease, error)
	DeleteLease(ctx context.Context, l *kube.Lease) error
}

// Timing bounds how long each phase may take.
type Timing struct {
	Poll           time.Duration // between checks
	DrainTimeout   time.Duration
	RestartTimeout time.Duration
	BootTimeout    time.Duration
	LockTTL        time.Duration // a crashed holder frees the lock after this
	RecoverTimeout time.Duration // Ceph back to clean after a host resume
	Total          time.Duration // hard limit for the whole action
}

// DefaultTiming matches scripts/k3s-roll.sh.
var DefaultTiming = Timing{
	Poll:           5 * time.Second,
	DrainTimeout:   5 * time.Minute,
	RestartTimeout: 6 * time.Minute,
	BootTimeout:    10 * time.Minute,
	LockTTL:        2 * time.Minute,
	RecoverTimeout: 20 * time.Minute,
	Total:          60 * time.Minute,
}

// Engine runs actions on this host.
type Engine struct {
	Cfg    *config.Config
	Kube   Kube
	Run    func(ctx context.Context, name string, args ...string) ([]byte, error)
	Log    *slog.Logger
	Timing Timing
	Now    func() time.Time
}

// Caller identifies who asked, for the lock and the audit record.
type Caller struct {
	Token     string // token name
	Scope     string
	RequestID string
}

// Errors the server maps to HTTP statuses.
var (
	ErrNotFound = errors.New("not found")
	// ErrPrecondition: the action would be unsafe now (412).
	ErrPrecondition = errors.New("precondition failed")
)

// BusyError: another action holds the lock (409).
type BusyError struct{ Holder string }

func (e *BusyError) Error() string { return "another action is running: " + e.Holder }

func preconditionf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrPrecondition, fmt.Sprintf(format, a...))
}

// Run is a started action. Events delivers progress; it is closed after the
// final event. The action keeps going when nobody reads: events then only go
// to the journal.
type Run struct {
	Action, Target string
	Started        time.Time

	events chan api.ActionEvent
	once   sync.Once
	done   chan struct{}
	err    error
}

// Events is the progress stream.
func (r *Run) Events() <-chan api.ActionEvent { return r.events }

// Wait blocks until the action finished and returns its error.
func (r *Run) Wait() error { <-r.done; return r.err }

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// start runs body in the background with its own deadline, independent of
// the request, and handles events, audit and the lock.
func (e *Engine) start(name, target string, c Caller, lk *lock, body func(ctx context.Context, emit func(string)) error, after ...func()) *Run {
	r := &Run{Action: name, Target: target, Started: e.now().UTC(), events: make(chan api.ActionEvent, 256), done: make(chan struct{})}
	log := e.Log.With("action", name, "target", target, "token", c.Token, "scope", c.Scope, "request_id", c.RequestID)
	send := func(status, msg string) {
		ev := api.ActionEvent{Time: e.now().UTC(), Status: status, Message: msg}
		select {
		case r.events <- ev:
		default: // nobody reading; the journal still has it
		}
	}
	emit := func(msg string) {
		log.Info("action progress", "message", msg)
		send(api.EventProgress, msg)
	}
	go func() {
		start := e.now()
		ctx, cancel := context.WithTimeout(context.Background(), e.Timing.Total)
		defer cancel()
		stopRenew := lk.keepAlive(ctx, e, log)
		err := body(ctx, emit)
		stopRenew()
		if rerr := lk.release(context.Background(), e); rerr != nil {
			log.Warn("releasing the lock failed; it expires by itself", "err", rerr)
		}
		d := e.now().Sub(start).Milliseconds()
		if err != nil {
			log.Error("audit", "event", "end", "result", "error", "error", err.Error(), "duration_ms", d)
			send(api.EventError, err.Error())
		} else {
			log.Info("audit", "event", "end", "result", "ok", "duration_ms", d)
			send(api.EventDone, name+" "+target+" done")
		}
		r.err = err
		close(r.events)
		close(r.done)
		if err == nil {
			for _, f := range after {
				f()
			}
		}
	}()
	log.Info("audit", "event", "start")
	return r
}
