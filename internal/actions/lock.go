package actions

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jurrebuunk/clusterd/internal/kube"
)

// One Lease for every disruptive action: two VM restarts at once could cost
// etcd quorum or capacity, whichever VMs they are.
const (
	lockNamespace = "clusterd"
	lockName      = "clusterd-disruption"
)

type lock struct {
	mu    sync.Mutex
	lease *kube.Lease
}

// currentHolder returns who holds the lock now, or "".
func (e *Engine) currentHolder(ctx context.Context) (string, error) {
	l, err := e.Kube.GetLease(ctx, lockNamespace, lockName)
	if err != nil || l == nil {
		return "", err
	}
	if e.expired(l) {
		return "", nil
	}
	return l.Spec.HolderIdentity, nil
}

func (e *Engine) expired(l *kube.Lease) bool {
	ttl := time.Duration(l.Spec.LeaseDurationSeconds) * time.Second
	return e.now().After(l.Spec.RenewTime.Add(ttl))
}

// acquire takes the lock or returns a *BusyError.
func (e *Engine) acquire(ctx context.Context, holder string) (*lock, error) {
	now := kube.MicroTime{Time: e.now()}
	ttl := int(e.Timing.LockTTL / time.Second)
	if ttl < 1 {
		ttl = 1
	}
	cur, err := e.Kube.GetLease(ctx, lockNamespace, lockName)
	if err != nil {
		return nil, err
	}
	var got *kube.Lease
	if cur == nil {
		l := &kube.Lease{}
		l.Metadata.Name, l.Metadata.Namespace = lockName, lockNamespace
		l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds = holder, ttl
		l.Spec.AcquireTime, l.Spec.RenewTime = now, now
		got, err = e.Kube.CreateLease(ctx, l)
	} else {
		if !e.expired(cur) && cur.Spec.HolderIdentity != "" {
			return nil, &BusyError{Holder: cur.Spec.HolderIdentity}
		}
		cur.Spec.HolderIdentity, cur.Spec.LeaseDurationSeconds = holder, ttl
		cur.Spec.AcquireTime, cur.Spec.RenewTime = now, now
		got, err = e.Kube.UpdateLease(ctx, cur)
	}
	if kube.IsStatus(err, http.StatusConflict) {
		// Someone else created or took it between our read and write.
		h, _ := e.currentHolder(ctx)
		return nil, &BusyError{Holder: h}
	}
	if err != nil {
		return nil, err
	}
	return &lock{lease: got}, nil
}

// keepAlive renews the lease until the returned stop is called.
func (l *lock) keepAlive(ctx context.Context, e *Engine, log *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(max(e.Timing.LockTTL/4, 10*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.mu.Lock()
				next := *l.lease
				next.Spec.RenewTime = kube.MicroTime{Time: e.now()}
				got, err := e.Kube.UpdateLease(ctx, &next)
				if err == nil {
					l.lease = got
				} else if !errors.Is(err, context.Canceled) {
					log.Warn("renewing the lock failed", "err", err)
				}
				l.mu.Unlock()
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (l *lock) release(ctx context.Context, e *Engine) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return e.Kube.DeleteLease(ctx, l.lease)
}
