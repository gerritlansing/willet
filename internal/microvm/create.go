package microvm

import (
	"context"
	"errors"
	"fmt"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// defaultCreateTimeout bounds creating one sandbox, including a first image
// pull, when Config.CreateTimeout is unset.
const defaultCreateTimeout = 10 * time.Minute

// When CreateSandbox is canceled part-way, microsandbox rolls the sandbox
// back to stopped rather than removing it, even if ephemeral, and it may stay
// until an unrelated sandbox start sweeps it up
// (superradcompany/microsandbox#1687, open as of v0.7.7). So creation is never
// canceled: it runs in the background, bounded only by its own timeout. The
// caller waits only as long as its context allows; if it stops waiting, or
// creation fails, it gets a pendingSandbox, an obligation to destroy whatever
// the creation leaves.

// creation is one sandbox creation running in the background.
type creation struct {
	name string
	done chan struct{}
	// Set before done is closed.
	sb  *msb.Sandbox
	err error
}

// createSandbox creates a sandbox, returning when it's ready or when ctx ends,
// whichever is first. If ctx ends first, or creation fails, it returns a
// leftover: the caller must keep it and destroy it, which waits for the
// creation to finish and removes any sandbox it produced.
func (p *Provisioner) createSandbox(ctx context.Context, name string, opts ...msb.SandboxOption) (sb *msb.Sandbox, leftover sandbox, err error) {
	c := &creation{name: name, done: make(chan struct{})}
	timeout := p.cfg.CreateTimeout
	if timeout <= 0 {
		timeout = defaultCreateTimeout
	}
	go func() {
		defer close(c.done)
		cctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c.sb, c.err = msb.CreateSandbox(cctx, name, opts...)
	}()

	pending := &pendingSandbox{p: p, c: c}
	select {
	case <-c.done:
	case <-ctx.Done():
		return nil, pending, ctx.Err()
	}
	if c.err == nil {
		return c.sb, nil, nil
	}

	// Creation failed, possibly at its own timeout; it may still have left a
	// sandbox. Clean up with a fresh context: ctx and the creation's own
	// context may both have expired.
	cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if derr := pending.Destroy(cleanCtx); derr != nil {
		return nil, pending, fmt.Errorf("%w (and removing the partly created sandbox failed, will retry: %v)", c.err, derr)
	}
	return nil, nil, c.err
}

// createTempSandbox is createSandbox for temporary VMs. It must be called
// under auxMu, from withTempVM's create step: a leftover becomes pendingAux,
// which blocks further temporary VMs until it is gone.
func (p *Provisioner) createTempSandbox(ctx context.Context, name string, opts ...msb.SandboxOption) (*msb.Sandbox, error) {
	sb, left, err := p.createSandbox(ctx, name, opts...)
	if left != nil {
		p.pendingAux = left
	}
	return sb, err
}

// errStillCreating means a pending sandbox's creation hasn't finished yet.
var errStillCreating = errors.New("sandbox is still being created")

// pendingSandbox is the teardown obligation for a creation the caller stopped
// waiting for, or one that failed and may have left a sandbox behind. It
// satisfies sandbox, so the usual retry paths (reclaim, pendingAux) own it.
type pendingSandbox struct {
	p *Provisioner
	c *creation
}

// Destroy waits for the creation to finish, within ctx, then removes what it
// produced. It returns nil only once no sandbox from this creation remains.
func (s *pendingSandbox) Destroy(ctx context.Context, opts ...msb.DestroyOption) error {
	select {
	case <-s.c.done:
	case <-ctx.Done():
		return fmt.Errorf("%s: %w: %w", s.c.name, errStillCreating, ctx.Err())
	}
	if s.c.sb != nil {
		err := s.c.sb.Destroy(ctx, opts...)
		if err != nil && !msb.IsKind(err, msb.ErrSandboxNotFound) {
			return err
		}
		return nil
	}
	return s.p.removePartial(ctx, s.c.name)
}

// Close releases the created sandbox's handle, if there is one.
func (s *pendingSandbox) Close() error {
	select {
	case <-s.c.done:
		if s.c.sb != nil {
			return s.c.sb.Close()
		}
	default:
	}
	return nil
}

// removePartial destroys a sandbox named name that this daemon owns, if one
// exists, after a creation failed. A lookup error is returned, not taken to
// mean no sandbox exists. Because the SDK's rollback of a canceled creation
// runs in the background, a "not found" is checked once more after a moment.
func (p *Provisioner) removePartial(ctx context.Context, name string) error {
	for attempt := 0; ; attempt++ {
		h, err := msb.GetSandbox(ctx, name)
		if msb.IsKind(err, msb.ErrSandboxNotFound) {
			if attempt > 0 {
				return nil
			}
			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err != nil {
			return fmt.Errorf("look up partly created sandbox %s: %w", name, err)
		}
		if cfg, err := h.Config(); err != nil || cfg.Labels[LabelOwner] != p.cfg.OwnerID {
			return nil // not ours to remove
		}
		return destroySandbox(ctx, handleSandbox{h})
	}
}

// handleSandbox lets a sandbox known only by its handle go through the same
// teardown as a connected one.
type handleSandbox struct{ h *msb.SandboxHandle }

func (s handleSandbox) Destroy(ctx context.Context, opts ...msb.DestroyOption) error {
	return s.h.Destroy(ctx, opts...)
}

func (handleSandbox) Close() error { return nil }
