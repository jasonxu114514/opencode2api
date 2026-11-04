package warp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

const DefaultPoolSize = 2

// egressCooldown keeps a public /64 (or IPv4 /32) that the upstream has
// rejected out of the pool for this long. The free tier scopes its quota to
// that block, so a new tunnel inside it would only repeat the same answer.
const egressCooldown = 30 * time.Minute

// refillRetryDelay spaces out a failed registration attempt. Successful
// registrations run back to back in the background.
const refillRetryDelay = 5 * time.Second

// A pool can only hold as many exits as Cloudflare hands out distinct egress
// blocks. When preload is larger than that supply, every extra registration is
// a duplicate; after a few in a row the pool stops trying for a while instead
// of hammering the registration API forever.
const (
	refillFailureThreshold = 3
	refillBackoffBase      = time.Minute
	refillBackoffMax       = 5 * time.Minute
)

// errDuplicateEgress reports a registration whose public egress block is
// already active or cooling down.
var errDuplicateEgress = errors.New("WARP egress is already active or cooling down")

// scaleDownDelay keeps exits created for a burst hot for a while after the
// burst, so a following burst reuses them instead of paying for new
// registrations.
const scaleDownDelay = 60 * time.Second

// ErrClosed is returned when a pool is used after Close. Callers use it to
// tell a retiring pool apart from a temporary capacity wait.
var ErrClosed = errors.New("WARP pool is closed")

type logger interface {
	Debugf(string, ...interface{})
	Infof(string, ...interface{})
	Warnf(string, ...interface{})
	Errorf(string, ...interface{})
}

// slogLogger adapts the structured logger used by the gateway to the short
// diagnostic messages this package emits at its own lifecycle boundaries.
type slogLogger struct {
	log *slog.Logger
}

func (l slogLogger) logf(level slog.Level, format string, args ...interface{}) {
	if l.log == nil {
		return
	}
	l.log.Log(context.Background(), level, fmt.Sprintf(format, args...), "component", "warp")
}

func (l slogLogger) Debugf(format string, args ...interface{}) {
	l.logf(slog.LevelDebug, format, args...)
}

func (l slogLogger) Infof(format string, args ...interface{}) {
	l.logf(slog.LevelInfo, format, args...)
}

func (l slogLogger) Warnf(format string, args ...interface{}) {
	l.logf(slog.LevelWarn, format, args...)
}

func (l slogLogger) Errorf(format string, args ...interface{}) {
	l.logf(slog.LevelError, format, args...)
}

type Pool struct {
	ctx     context.Context
	cancel  context.CancelFunc
	logger  logger
	size    int
	preload int
	stack   Stack
	newExit func(context.Context) (*exit, error)

	mu        sync.Mutex
	active    []*exit
	inflight  int
	waiting   int
	next      int
	reserved  map[string]bool
	cooling   map[string]time.Time
	changed   chan struct{}
	closed    bool
	refilling bool
	scaling   bool
	// refillFailures counts consecutive failed preload registrations;
	// refillPaused keeps every refill trigger quiet until it expires.
	refillFailures int
	refillPaused   time.Time
	wait           sync.WaitGroup
	createMu       sync.Mutex
}

type Lease struct {
	pool *Pool
	exit *exit
	once sync.Once
}

// Snapshot is a point-in-time view of a pool for health and admin reporting.
type Snapshot struct {
	Stack string
	Size  int
	// Preload is the number of exits kept hot even when the pool is idle:
	// half of Size (at least one), registered in the background after the
	// first exit so a burst does not wait for a registration.
	Preload int
	// Active is the number of exits currently in the ring. Standby counts the
	// idle part of it: exits not held by a lease and ready to replace a
	// penalized one immediately.
	Active  int
	Standby int
	Cooling int
	Egress  []string
}

// Options configure a WARP pool.
type Options struct {
	// Size is the upper bound on exits. Half of it (at least one) is kept hot
	// as a preload even while the pool is idle; beyond that the pool keeps as
	// many exits hot as the current concurrency needs and never exceeds Size.
	Size int
	// Stack selects the address family inside the tunnel. An IPv6 stack makes
	// the public egress an IPv6 address.
	Stack Stack
	// SourceIPv6, when valid, is the outer IPv6 prefix each exit binds its
	// WireGuard socket to. Every exit draws its own address from it, so a
	// replacement reaches Cloudflare from a new source and receives a new
	// public egress — the only way past an egress-scoped free usage limit.
	SourceIPv6 netip.Prefix
}

// NewWithOptions creates a WARP pool whose exits use the selected IP stack and,
// optionally, rotate their outer IPv6 source within a prefix.
func NewWithOptions(ctx context.Context, log *slog.Logger, opts Options) (*Pool, error) {
	normalized, err := ParseStack(string(opts.Stack))
	if err != nil {
		return nil, err
	}
	if opts.SourceIPv6.IsValid() && (!opts.SourceIPv6.Addr().Is6() || opts.SourceIPv6.Bits() > 64) {
		return nil, fmt.Errorf("WARP outer source must be an IPv6 prefix of /64 or shorter, got %s", opts.SourceIPv6)
	}
	size := opts.Size
	if size <= 0 {
		size = DefaultPoolSize
	}
	r := newRegistrar()
	pool, err := newPool(ctx, slogLogger{log: log}, size, func(ctx context.Context) (*exit, error) {
		reg, err := r.register(ctx)
		if err != nil {
			return nil, err
		}
		var source netip.Addr
		if opts.SourceIPv6.IsValid() {
			source, err = randomSource(opts.SourceIPv6)
			if err != nil {
				return nil, err
			}
		}
		return newExitWithStack(ctx, reg, normalized, source)
	})
	if err != nil {
		return nil, err
	}
	pool.stack = normalized
	return pool, nil
}

// randomSource draws one host address from prefix. The bits above the prefix
// length are kept, every bit below is random, so two exits practically never
// share an outer source.
func randomSource(prefix netip.Prefix) (netip.Addr, error) {
	address := prefix.Masked().Addr().As16()
	var noise [16]byte
	if _, err := rand.Read(noise[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("draw WARP outer source: %w", err)
	}
	keep := prefix.Bits()
	for index := range address {
		switch bits := keep - index*8; {
		case bits >= 8:
		case bits <= 0:
			address[index] = noise[index]
		default:
			mask := byte(0xFF) << (8 - bits)
			address[index] = address[index]&mask | noise[index]&^mask
		}
	}
	return netip.AddrFrom16(address), nil
}

func newPool(ctx context.Context, log logger, size int, create func(context.Context) (*exit, error)) (*Pool, error) {
	if size <= 0 {
		size = DefaultPoolSize
	}
	// Keep half of the ceiling warm (at least one) so a burst finds ready
	// exits instead of paying for registrations while requests queue. The
	// first exit is synchronous and gates readiness; the rest register in the
	// background.
	preload := size / 2
	if preload < 1 {
		preload = 1
	}
	// The caller's context bounds the synchronous first registration, not the
	// pool: it is typically an initialization deadline that expires moments
	// after NewWithOptions returns. Deriving the pool lifetime from it would
	// cancel every later refill and replacement with "context canceled",
	// leaving the pool stuck on its first egress forever.
	poolCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Pool{
		ctx:      poolCtx,
		cancel:   cancel,
		logger:   log,
		size:     size,
		preload:  preload,
		newExit:  create,
		reserved: make(map[string]bool),
		cooling:  make(map[string]time.Time),
		changed:  make(chan struct{}),
	}
	// The first registration gates readiness. Cloudflare can answer it with a
	// rate limit even when this process has just started (the limit follows the
	// source address), so wait out that window inside the caller's deadline
	// instead of failing startup over a pause that is about to end. Any other
	// failure still fails closed.
	first, err := p.createExit(ctx)
	for err != nil {
		pause := registrationPause(err)
		if pause <= 0 {
			cancel()
			return nil, err
		}
		if log != nil {
			log.Infof("OpenCode WARP first registration paused for %s", pause.Round(time.Second))
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			cancel()
			return nil, ctx.Err()
		case <-timer.C:
		}
		first, err = p.createExit(ctx)
	}
	p.active = []*exit{first}
	if log != nil {
		log.Infof("OpenCode WARP/%s pool ready (max %d exits, preloading %d)", first.stack, size, preload)
	}
	p.startRefill()
	return p, nil
}

// desiredLocked is the number of exits the pool should keep hot: one per
// in-flight lease, one for every request waiting for an exit, plus a spare, so
// a 429 can switch to a ready exit instead of waiting for a registration. The
// preload floor keeps half the ceiling registered while the pool is idle.
// Size is the upper bound; at idle a pool of two or more keeps one spare.
func (p *Pool) desiredLocked() int {
	want := p.inflight + p.waiting + 1
	if want < p.preload {
		want = p.preload
	}
	if want < 2 {
		want = 2
	}
	if want > p.size {
		want = p.size
	}
	return want
}

func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		for attempts := 0; attempts < len(p.active); attempts++ {
			index := p.next % len(p.active)
			p.next = (p.next + 1) % len(p.active)
			active := p.active[index]
			if active.addRef() {
				p.inflight++
				p.mu.Unlock()
				// Demand just rose: grow the ring toward it in the background.
				p.startRefill()
				return &Lease{pool: p, exit: active}, nil
			}
		}
		p.waiting++
		changed := p.changed
		needRefill := len(p.active) < p.desiredLocked()
		p.mu.Unlock()
		if needRefill {
			// Queued demand is demand too: start growing before we block.
			p.startRefill()
		}
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-p.ctx.Done():
			err = ErrClosed
		case <-changed:
		}
		p.mu.Lock()
		p.waiting--
		p.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (p *Pool) Report(lease *Lease, status int, body []byte) {
	if lease == nil || lease.exit == nil {
		return
	}
	rotate, reason := lease.exit.observe(status, body)
	if !rotate {
		return
	}
	p.penalize(lease.exit, reason)
}

// Fail drops an exit that cannot carry traffic even though the upstream never
// produced an HTTP response, such as a broken WARP handshake or a stalled first
// token. Retrying the same user-space tunnel would only repeat the stall.
func (p *Pool) Fail(lease *Lease, reason string) {
	if lease == nil || lease.exit == nil || reason == "" {
		return
	}
	p.penalize(lease.exit, reason)
}

func (p *Pool) Size() int { return p.size }

// Snapshot reports the pool's current capacity and the public egress address
// of every active exit. It never exposes keys or device tokens.
func (p *Pool) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := Snapshot{Stack: string(p.stack), Size: p.size, Preload: p.preload, Active: len(p.active), Cooling: len(p.cooling)}
	for _, active := range p.active {
		if active.idle() {
			snapshot.Standby++
		}
		if active.egress.IsValid() {
			snapshot.Egress = append(snapshot.Egress, active.egress.String())
		}
	}
	return snapshot
}

func (p *Pool) penalize(bad *exit, reason string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	index := p.activeIndexLocked(bad)
	if index < 0 {
		p.mu.Unlock()
		return
	}
	if !bad.markPenalized() {
		p.mu.Unlock()
		return
	}
	if key := bad.egressKey(); key != "" {
		p.cooling[key] = time.Now().Add(egressCooldown)
		delete(p.reserved, key)
	}
	p.active = append(p.active[:index], p.active[index+1:]...)
	if p.next >= len(p.active) {
		p.next = 0
	}
	p.notifyLocked()
	p.wait.Add(1)
	p.mu.Unlock()

	if p.logger != nil {
		p.logger.Warnf("Rotating OpenCode WARP egress %s (%s)", bad.id, reason)
	}
	// Re-register immediately: the retired exit is replaced in the background
	// and the retry draws whichever active exit is ready first.
	go func() {
		defer p.wait.Done()
		p.replace(bad, reason)
	}()
}

func (p *Pool) replace(old *exit, reason string) {
	replacement, err := p.createExit(p.ctx)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		if replacement != nil {
			p.releaseReservation(replacement)
			replacement.close()
		}
		old.retire()
		return
	}
	if err != nil {
		restored := len(p.active) == 0
		if restored {
			old.clearPenalty()
			if key := old.egressKey(); key != "" {
				p.reserved[key] = true
				delete(p.cooling, key)
			}
			p.active = append(p.active, old)
			p.notifyLocked()
		}
		p.mu.Unlock()
		if !restored {
			old.retire()
		}
		if p.logger != nil {
			if pause := registrationPause(err); pause > 0 {
				p.logger.Infof("OpenCode WARP replacement paused for %s", pause.Round(time.Second))
			} else {
				p.logger.Errorf("Cannot replace OpenCode WARP egress %s: %v", old.id, err)
			}
		}
		p.startRefill()
		return
	}
	if p.activeIndexLocked(old) >= 0 {
		p.mu.Unlock()
		p.releaseReservation(replacement)
		replacement.close()
		old.retire()
		return
	}
	placed := false
	if len(p.active) < p.size {
		p.active = append(p.active, replacement)
		placed = true
	}
	p.notifyLocked()
	p.mu.Unlock()
	if !placed {
		p.releaseReservation(replacement)
		replacement.close()
		old.retire()
		return
	}
	if p.logger != nil {
		p.logger.Infof("OpenCode WARP egress %s replaced by %s at %s (%s), active=%d", old.id, replacement.id, replacement.egress, reason, len(p.active))
	}
	old.retire()
	p.startRefill()
}

func (p *Pool) refill() {
	p.mu.Lock()
	if p.closed || p.refilling || time.Now().Before(p.refillPaused) || !p.needsRefillLocked() {
		p.mu.Unlock()
		return
	}
	p.refilling = true
	p.mu.Unlock()

	replacement, err := p.createExit(p.ctx)
	p.mu.Lock()
	p.refilling = false
	placed := false
	if err == nil && !p.closed && len(p.active) < p.size {
		p.active = append(p.active, replacement)
		replacement = nil
		placed = true
	}
	if err == nil {
		p.refillFailures = 0
		p.refillPaused = time.Time{}
	}
	if placed || p.closed {
		p.notifyLocked()
	}
	p.mu.Unlock()

	if replacement != nil {
		p.releaseReservation(replacement)
		replacement.close()
	}
	if err != nil && p.ctx.Err() == nil {
		delay := refillRetryDelay
		if pause := registrationPause(err); pause > 0 {
			// Cloudflare's rate limit is the pacing: wait out the window it
			// asked for instead of retrying into the same answer.
			delay = pause
			if delay < refillRetryDelay {
				delay = refillRetryDelay
			}
			if delay > maxRateLimitPause {
				delay = maxRateLimitPause
			}
			if p.logger != nil {
				p.logger.Infof("OpenCode WARP registration paused for %s", delay.Round(time.Second))
			}
		} else if errors.Is(err, errDuplicateEgress) {
			p.mu.Lock()
			p.refillFailures++
			failures := p.refillFailures
			p.mu.Unlock()
			if failures >= refillFailureThreshold {
				// The egress supply is smaller than the target. Back off
				// exponentially so a saturated colo is not registered into
				// forever.
				shift := failures - refillFailureThreshold
				if shift > 3 {
					shift = 3
				}
				delay = refillBackoffBase << shift
				if delay > refillBackoffMax {
					delay = refillBackoffMax
				}
				if p.logger != nil {
					p.logger.Infof("OpenCode WARP preload paused for %s after %d duplicate egresses", delay.Round(time.Second), failures)
				}
			}
		} else if p.logger != nil {
			p.logger.Warnf("Cannot pre-register OpenCode WARP egress: %v", err)
		}
		p.mu.Lock()
		if paused := time.Now().Add(delay); paused.After(p.refillPaused) {
			p.refillPaused = paused
		}
		p.mu.Unlock()
		p.after(delay, p.retryRefill)
		return
	}
	p.startRefill()
}

// retryRefill is the timer half of a failed refill: it clears the pause that
// kept ordinary triggers quiet and asks for another attempt. Going through
// startRefill (rather than calling refill directly) keeps the single-flight
// guard.
func (p *Pool) retryRefill() {
	p.mu.Lock()
	p.refillPaused = time.Time{}
	p.mu.Unlock()
	p.startRefill()
}

func (p *Pool) startRefill() {
	p.mu.Lock()
	if p.closed || p.refilling || time.Now().Before(p.refillPaused) || !p.needsRefillLocked() {
		p.mu.Unlock()
		return
	}
	p.wait.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wait.Done()
		p.refill()
	}()
}

// released records that a lease was returned and trims the ring back toward
// the demand of the moment. Freed exits stay hot for scaleDownDelay, so a
// burst that pauses does not immediately pay for re-registering them.
func (p *Pool) released() {
	p.mu.Lock()
	if p.inflight > 0 {
		p.inflight--
	}
	closed := p.closed
	wantRefill := !closed && len(p.active) < p.desiredLocked()
	started := p.scaling
	if !closed && !started && len(p.active) > p.desiredLocked() {
		p.scaling = true
		started = true
	}
	p.mu.Unlock()
	if closed {
		return
	}
	if wantRefill {
		p.startRefill()
	}
	if started {
		p.after(scaleDownDelay, p.scaleDown)
	}
}

// scaleDown retires every idle exit above the current demand. Busy surplus
// exits are left to their release, which arms another pass if the ring is
// still over target then.
func (p *Pool) scaleDown() {
	p.mu.Lock()
	p.scaling = false
	if p.closed {
		p.mu.Unlock()
		return
	}
	want := p.desiredLocked()
	var dropped []*exit
	if surplus := len(p.active) - want; surplus > 0 {
		kept := p.active[:0]
		for _, candidate := range p.active {
			if surplus > 0 && candidate.idle() {
				dropped = append(dropped, candidate)
				surplus--
				continue
			}
			kept = append(kept, candidate)
		}
		p.active = kept
	}
	if p.next >= len(p.active) {
		p.next = 0
	}
	p.mu.Unlock()
	for _, drop := range dropped {
		p.releaseReservation(drop)
		drop.retire()
		if p.logger != nil {
			p.logger.Infof("OpenCode WARP egress %s scaled down at %s", drop.id, drop.egress)
		}
	}
}

// after runs fn once delay has elapsed, unless the pool closes first. The wait
// group is incremented under the pool lock so it cannot race with Close's
// Wait: a timer scheduled after Close is not started at all, and one scheduled
// before it is counted before Close can begin waiting.
func (p *Pool) after(delay time.Duration, fn func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.wait.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wait.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-p.ctx.Done():
		case <-timer.C:
			fn()
		}
	}()
}

func (p *Pool) createExit(ctx context.Context) (*exit, error) {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		candidate, err := p.newExit(ctx)
		if err != nil {
			return nil, err
		}
		key := candidate.egressKey()
		now := time.Now()
		p.mu.Lock()
		for subnet, until := range p.cooling {
			if !until.After(now) {
				delete(p.cooling, subnet)
			}
		}
		usable := key != "" && !p.reserved[key] && !p.cooling[key].After(now)
		if usable {
			p.reserved[key] = true
		}
		p.mu.Unlock()
		if usable {
			if p.logger != nil {
				p.logger.Infof("OpenCode WARP/%s egress %s ready at %s", candidate.stack, candidate.id, candidate.egress)
			}
			return candidate, nil
		}
		lastErr = fmt.Errorf("%w: %s", errDuplicateEgress, key)
		if p.logger != nil {
			p.logger.Warnf("Discarding duplicate WARP exit %s: %v", candidate.id, lastErr)
		}
		candidate.close()
	}
	return nil, lastErr
}

func (p *Pool) releaseReservation(e *exit) {
	if e == nil {
		return
	}
	key := e.egressKey()
	if key == "" {
		return
	}
	p.mu.Lock()
	delete(p.reserved, key)
	p.mu.Unlock()
}

func (p *Pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Pool) activeIndexLocked(target *exit) int {
	for index, candidate := range p.active {
		if candidate == target {
			return index
		}
	}
	return -1
}

func (p *Pool) needsRefillLocked() bool {
	return len(p.active) < p.desiredLocked()
}

func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	active := p.active
	p.active = nil
	p.notifyLocked()
	p.mu.Unlock()

	p.cancel()
	for _, item := range active {
		p.releaseReservation(item)
		item.retire()
	}
	p.wait.Wait()
}

func (l *Lease) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if l == nil || l.exit == nil {
		return nil, errors.New("WARP lease is closed")
	}
	return l.exit.dialContext(ctx, network, address)
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.exit != nil {
			l.exit.release()
		}
		if l.pool != nil {
			l.pool.released()
		}
	})
}

func (l *Lease) EgressKey() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.egressKey()
}

// DialNetwork is the explicit TCP address family for this lease. Callers should
// pass it to DialContext instead of assuming IPv6.
func (l *Lease) DialNetwork() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.dialNetwork()
}

func (l *Lease) String() string {
	if l == nil || l.exit == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s", l.exit.id, l.exit.egress)
}
