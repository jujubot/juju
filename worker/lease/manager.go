// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package lease

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juju/clock"
	"github.com/juju/errors"
	"github.com/juju/worker/v2/catacomb"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/retry.v1"

	"github.com/juju/juju/core/lease"
)

const (
	// maxRetries gives the maximum number of attempts we'll try if
	// there are timeouts.
	maxRetries = 10

	// initialRetryDelay is the starting delay - this will be
	// increased exponentially up maxRetries.
	initialRetryDelay = 50 * time.Millisecond

	// retryBackoffFactor is how much longer we wait after a failing retry.
	// Retrying 10 times starting at 50ms and backing off 1.6x gives us a total
	// delay time of about 9s.
	retryBackoffFactor = 1.6

	// maxShutdownWait is the maximum time to wait for the async
	// claims and expires to complete before stopping the worker
	// anyway. Picked to be slightly quicker than the httpserver
	// shutdown timeout.
	maxShutdownWait = 55 * time.Second
)

// errStopped is returned to clients when an operation cannot complete because
// the manager has started (and possibly finished) shutdown.
var errStopped = errors.New("lease manager stopped")

type dummySecretary struct{}

func (d dummySecretary) CheckLease(key lease.Key) error             { return nil }
func (d dummySecretary) CheckHolder(name string) error              { return nil }
func (d dummySecretary) CheckDuration(duration time.Duration) error { return nil }

// NewDeadManager returns a manager that's already dead
// and always returns the given error.
func NewDeadManager(err error) *Manager {
	var secretary dummySecretary
	m := Manager{
		config: ManagerConfig{
			Secretary: func(_ string) (Secretary, error) {
				return secretary, nil
			},
		},
	}
	_ = catacomb.Invoke(catacomb.Plan{
		Site: &m.catacomb,
		Work: func() error {
			return errors.Trace(err)
		},
	})
	return &m
}

// NewManager returns a new *Manager configured as supplied. The caller takes
// responsibility for killing, and handling errors from, the returned Worker.
func NewManager(config ManagerConfig) (*Manager, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	logContext := config.EntityUUID
	if len(logContext) > 6 {
		logContext = logContext[:6]
	}
	manager := &Manager{
		config:     config,
		claims:     make(chan claim),
		revokes:    make(chan revoke),
		checks:     make(chan check),
		blocks:     make(chan block),
		expireDone: make(chan struct{}),
		pins:       make(chan pin),
		unpins:     make(chan pin),
		logContext: logContext,
	}
	err := catacomb.Invoke(catacomb.Plan{
		Site: &manager.catacomb,
		Work: manager.loop,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return manager, nil
}

// Manager implements worker.Worker and can be bound to get
// lease.Checkers and lease.Claimers.
type Manager struct {
	catacomb catacomb.Catacomb

	// config collects all external configuration and dependencies.
	config ManagerConfig

	// logContext is just a string that associates messages in the log
	// It is seeded with the first six characters of the config.EntityUUID
	// if supplied
	logContext string

	// nextTimeout is the next time that has a possible expiry that we would care
	// about, capped at the maximum time.
	nextTimeout time.Time

	// timer tracks when nextTimeout would expire and triggers when it does
	timer clock.Timer

	// muNextTimeout protects accesses to nextTimeout
	muNextTimeout sync.Mutex

	// claims is used to deliver lease claim requests to the loop.
	claims chan claim

	// revokes is used to deliver lease revoke requests to the loop.
	revokes chan revoke

	// checks is used to deliver lease check requests to the loop.
	checks chan check

	// expireDone is sent an event when we successfully finish a call to expire()
	expireDone chan struct{}

	// blocks is used to deliver expiry block requests to the loop.
	blocks chan block

	// pins is used to deliver lease pin requests to the loop.
	pins chan pin

	// unpins is used to deliver lease unpin requests to the loop.
	unpins chan pin

	// wg is used to ensure that all child goroutines are finished
	// before we stop.
	wg sync.WaitGroup

	// outstandingClaims tracks how many unfinished claim goroutines
	// are running (for debugging purposes).
	outstandingClaims int64

	// outstandingRevokes tracks how many unfinished revoke goroutines
	// are running (for debugging purposes).
	outstandingRevokes int64
}

// Kill is part of the worker.Worker interface.
func (manager *Manager) Kill() {
	manager.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (manager *Manager) Wait() error {
	return manager.catacomb.Wait()
}

// loop runs until the manager is stopped.
func (manager *Manager) loop() error {
	if collector, ok := manager.config.Store.(prometheus.Collector); ok && manager.config.PrometheusRegisterer != nil {
		// The store implements the collector interface, but the lease.Store
		// does not expose those.
		_ = manager.config.PrometheusRegisterer.Register(collector)
		defer manager.config.PrometheusRegisterer.Unregister(collector)
	}

	defer manager.waitForGoroutines()
	blocks := make(blocks)
	manager.setupInitialTimer()
	for {
		if err := manager.choose(blocks); err != nil {
			manager.config.Logger.Tracef("[%s] exiting main loop with error: %v", manager.logContext, err)
			return errors.Trace(err)
		}
	}
}

func (manager *Manager) lookupLease(leaseKey lease.Key) (lease.Info, bool) {
	l, exists := manager.config.Store.Leases(leaseKey)[leaseKey]
	return l, exists
}

// choose breaks the select out of loop to make the blocking logic clearer.
func (manager *Manager) choose(blocks blocks) error {
	select {
	case <-manager.catacomb.Dying():
		return manager.catacomb.ErrDying()
	case check := <-manager.checks:
		return manager.handleCheck(check)
	case now := <-manager.timer.Chan():
		manager.tick(now, blocks)
	case <-manager.expireDone:
		manager.checkBlocks(blocks)
	case claim := <-manager.claims:
		manager.startingClaim()
		go manager.retryingClaim(claim)
	case revoke := <-manager.revokes:
		manager.startingRevoke()
		go manager.retryingRevoke(revoke)
	case pin := <-manager.pins:
		manager.handlePin(pin)
	case unpin := <-manager.unpins:
		manager.handleUnpin(unpin)
	case block := <-manager.blocks:
		// TODO(raftlease): Include the other key items.
		manager.config.Logger.Tracef("[%s] adding block for: %s", manager.logContext, block.leaseKey.Lease)
		blocks.add(block)

		if _, exists := manager.lookupLease(block.leaseKey); !exists {
			// Nobody holds this lease, so immediately unblock it.
			blocks.unblock(block.leaseKey)
		}
	}
	return nil
}

func (manager *Manager) bind(namespace, modelUUID string) (broker, error) {
	secretary, err := manager.config.Secretary(namespace)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &boundManager{
		manager:   manager,
		secretary: secretary,
		namespace: namespace,
		modelUUID: modelUUID,
	}, nil
}

// Checker returns a lease.Checker for the specified namespace and model.
func (manager *Manager) Checker(namespace, modelUUID string) (lease.Checker, error) {
	return manager.bind(namespace, modelUUID)
}

// Claimer returns a lease.Claimer for the specified namespace and model.
func (manager *Manager) Claimer(namespace, modelUUID string) (lease.Claimer, error) {
	return manager.bind(namespace, modelUUID)
}

// Revoker returns a lease.Revoker for the specified namespace and model.
func (manager *Manager) Revoker(namespace, modelUUID string) (lease.Revoker, error) {
	return manager.bind(namespace, modelUUID)
}

// Pinner returns a lease.Pinner for the specified namespace and model.
func (manager *Manager) Pinner(namespace, modelUUID string) (lease.Pinner, error) {
	return manager.bind(namespace, modelUUID)
}

// Reader returns a lease.Reader for the specified namespace and model.
func (manager *Manager) Reader(namespace, modelUUID string) (lease.Reader, error) {
	return manager.bind(namespace, modelUUID)
}

// retryingClaim handles timeouts when claiming, and responds to the
// claiming party when it eventually succeeds or fails, or if it times
// out after a number of retries.
func (manager *Manager) retryingClaim(claim claim) {
	defer manager.finishedClaim()
	var (
		err     error
		success bool
	)
	for a := manager.startRetry(); a.Next(); {
		success, err = manager.handleClaim(claim)
		if !lease.IsTimeout(err) && !lease.IsInvalid(err) {
			break
		}
		if a.More() {
			if lease.IsInvalid(err) {
				manager.config.Logger.Tracef("[%s] request by %s for lease %s %v, retrying...",
					manager.logContext, claim.holderName, claim.leaseKey.Lease, err)
			} else {
				manager.config.Logger.Tracef("[%s] timed out handling claim by %s for lease %s, retrying...",
					manager.logContext, claim.holderName, claim.leaseKey.Lease)
			}
		}
	}

	if err == nil {
		if !success {
			claim.respond(lease.ErrClaimDenied)
			return
		}
		// now, this isn't strictly true, as the lease can be given for longer
		// than the requested duration. However, it cannot be shorter.
		// Doing it this way, we'll wake up, and then see we can sleep
		// for a bit longer. But we'll always wake up in time.
		manager.ensureNextTimeout(claim.duration)
		// respond after ensuring the timeout, at least for the test suite to
		// be sure the timer has been updated by the time it gets Claim()
		// to return.
		claim.respond(nil)
	} else {
		switch {
		case lease.IsTimeout(err):
			claim.respond(lease.ErrTimeout)
			manager.config.Logger.Warningf("[%s] retrying timed out while handling claim %q for %q",
				manager.logContext, claim.leaseKey, claim.holderName)
		case lease.IsInvalid(err):
			// We want to see this, but it doesn't indicate something a user
			// can do something about.
			manager.config.Logger.Infof("[%s] got %v after %d retries, denying claim %q for %q",
				manager.logContext, err, maxRetries, claim.leaseKey, claim.holderName)
			claim.respond(lease.ErrClaimDenied)
		case lease.IsHeld(err):
			// This can happen in HA if the original check for an extant lease
			// (against the local node) returned nothing, but the leader FSM
			// has this lease being held by another entity.
			manager.config.Logger.Tracef(
				"[%s] %s asked for lease %s, held by by another entity; local Raft node may be syncing",
				manager.logContext, claim.holderName, claim.leaseKey.Lease)
			claim.respond(lease.ErrClaimDenied)
		default:
			// Stop the main loop because we got an abnormal error
			manager.catacomb.Kill(errors.Trace(err))
		}
	}
}

// handleClaim processes the supplied claim. It will only return
// unrecoverable errors or timeouts; mere failure to claim just
// indicates a bad request, and is returned as (false, nil).
func (manager *Manager) handleClaim(claim claim) (bool, error) {
	store := manager.config.Store
	request := lease.Request{Holder: claim.holderName, Duration: claim.duration}
	var err error
	var action string
	select {
	case <-manager.catacomb.Dying():
		return false, manager.catacomb.ErrDying()
	default:
		info, found := manager.lookupLease(claim.leaseKey)
		switch {
		case !found:
			manager.config.Logger.Tracef("[%s] %s asked for lease %s, no lease found, claiming for %s",
				manager.logContext, claim.holderName, claim.leaseKey.Lease, claim.duration)
			action = "claiming"
			err = store.ClaimLease(claim.leaseKey, request, manager.catacomb.Dying())
		case info.Holder == claim.holderName:
			manager.config.Logger.Tracef("[%s] %s extending lease %s for %s",
				manager.logContext, claim.holderName, claim.leaseKey.Lease, claim.duration)
			action = "extending"
			err = store.ExtendLease(claim.leaseKey, request, manager.catacomb.Dying())
		default:
			// Note: (jam) 2017-10-31) We don't check here if the lease has
			// expired for the current holder. Should we?
			remaining := info.Expiry.Sub(manager.config.Clock.Now())
			manager.config.Logger.Tracef("[%s] %s asked for lease %s, held by %s for another %s, rejecting",
				manager.logContext, claim.holderName, claim.leaseKey.Lease, info.Holder, remaining)
			return false, nil
		}
	}
	if lease.IsAborted(err) {
		return false, manager.catacomb.ErrDying()
	}
	if err != nil {
		return false, errors.Trace(err)
	}
	manager.config.Logger.Tracef("[%s] %s %s lease %s for %s successful",
		manager.logContext, claim.holderName, action, claim.leaseKey.Lease, claim.duration)
	return true, nil
}

// retryingRevoke handles timeouts when revoking, and responds to the
// revoking party when it eventually succeeds or fails, or if it times
// out after a number of retries.
func (manager *Manager) retryingRevoke(revoke revoke) {
	defer manager.finishedRevoke()
	var err error
	for a := manager.startRetry(); a.Next(); {
		err = manager.handleRevoke(revoke)
		if !lease.IsTimeout(err) && !lease.IsInvalid(err) {
			break
		}
		if a.More() {
			if lease.IsInvalid(err) {
				manager.config.Logger.Tracef("[%s] request by %s for revoking lease %s %v, retrying...",
					manager.logContext, revoke.holderName, revoke.leaseKey.Lease, err)
			} else {
				manager.config.Logger.Tracef("[%s] timed out handling revoke by %s for lease %s, retrying...",
					manager.logContext, revoke.holderName, revoke.leaseKey.Lease)
			}
		}
	}

	if err == nil {
		revoke.respond(nil)
		// If we send back an error, then the main loop won't listen for expireDone
		select {
		case <-manager.catacomb.Dying():
			return
		case manager.expireDone <- struct{}{}:
		}
	} else {
		switch {
		case lease.IsTimeout(err):
			manager.config.Logger.Warningf("[%s] retrying timed out while handling revoke %q for %q",
				manager.logContext, revoke.leaseKey, revoke.holderName)
			revoke.respond(lease.ErrTimeout)
		case lease.IsInvalid(err):
			// we want to see this, but it doesn't indicate something a user can do something about
			manager.config.Logger.Infof("[%s] got %v after %d retries, revoke %q for %q",
				manager.logContext, err, maxRetries, revoke.leaseKey, revoke.holderName)
			revoke.respond(err)
		case lease.IsNotHeld(err):
			// we want to see this, but it doesn't indicate something a user can do something about
			manager.config.Logger.Infof("[%s] got %v after %d retries, revoke %q for %q",
				manager.logContext, err, maxRetries, revoke.leaseKey, revoke.holderName)
			revoke.respond(err)
		default:
			// Stop the main loop because we got an abnormal error
			manager.catacomb.Kill(errors.Trace(err))
		}
	}
}

// handleRevoke processes the supplied revocation. It will only return
// unrecoverable errors or timeouts.
func (manager *Manager) handleRevoke(revoke revoke) error {
	store := manager.config.Store
	var err error
	select {
	case <-manager.catacomb.Dying():
		return manager.catacomb.ErrDying()
	default:
		info, found := manager.lookupLease(revoke.leaseKey)
		switch {
		case !found:
			manager.config.Logger.Tracef("[%s] %s asked to revoke lease %s, no lease found",
				manager.logContext, revoke.holderName, revoke.leaseKey.Lease)
			return nil
		case info.Holder == revoke.holderName:
			manager.config.Logger.Tracef("[%s] %s revoking lease %s",
				manager.logContext, revoke.holderName, revoke.leaseKey.Lease)
			err = store.RevokeLease(revoke.leaseKey, revoke.holderName, manager.catacomb.Dying())
		default:
			manager.config.Logger.Tracef("[%s] %s revoking lease %s, held by %s, rejecting",
				manager.logContext, revoke.holderName, revoke.leaseKey.Lease, info.Holder)
			return lease.ErrNotHeld
		}
	}
	if lease.IsAborted(err) {
		return manager.catacomb.ErrDying()
	}
	if err != nil {
		return errors.Trace(err)
	}
	manager.config.Logger.Tracef("[%s] %s revoked lease %s successful",
		manager.logContext, revoke.holderName, revoke.leaseKey.Lease)
	return nil
}

// handleCheck processes and responds to the supplied check. It will only return
// unrecoverable errors; mere untruth of the assertion just indicates a bad
// request, and is communicated back to the check's originator.
func (manager *Manager) handleCheck(check check) error {
	key := check.leaseKey

	manager.config.Logger.Tracef("[%s] handling Check for lease %s on behalf of %s",
		manager.logContext, key.Lease, check.holderName)

	info, found := manager.lookupLease(key)

	var response error
	if !found || info.Holder != check.holderName {
		if found {
			manager.config.Logger.Tracef("[%s] handling Check for lease %s on behalf of %s, found held by %s",
				manager.logContext, key.Lease, check.holderName, info.Holder)
		} else {
			// Someone thought they were the leader and held a Claim or they wouldn't
			// have tried to do a mutating operation. However, when they actually
			// got to this point, we detected that they were, actually, out of
			// date. Schedule a sync
			manager.ensureNextTimeout(time.Millisecond)

			manager.config.Logger.Tracef("[%s] handling Check for lease %s on behalf of %s, not found",
				manager.logContext, key.Lease, check.holderName)
		}

		response = lease.ErrNotHeld
	} else if check.trapdoorKey != nil {
		response = info.Trapdoor(check.attempt, check.trapdoorKey)
	}
	check.respond(errors.Trace(response))
	return nil
}

// tick triggers when we think a lease might be expiring, so we check if there
// are leases to expire, and then unblock anything that is no longer blocked,
// and then compute the next time we should wake up.
func (manager *Manager) tick(now time.Time, blocks blocks) {
	manager.config.Logger.Tracef("[%s] tick at %v, running expiry checks\n", manager.logContext, now)
	// Check for blocks that need to be notified.
	manager.checkBlocks(blocks)
}

func (manager *Manager) checkBlocks(blocks blocks) {
	now := manager.config.Clock.Now()
	manager.config.Logger.Tracef("[%s] evaluating %d blocks", manager.logContext, len(blocks))
	leases := manager.config.Store.Leases()
	for leaseName := range blocks {
		if _, found := leases[leaseName]; !found {
			manager.config.Logger.Tracef("[%s] unblocking: %s", manager.logContext, leaseName)
			blocks.unblock(leaseName)
		}
	}
	manager.computeNextTimeout(now, leases)
}

// computeNextTimeout iterates the leases and finds out what the next time we
// want to wake up, expire any leases and then handle any unblocks that happen.
// It is based on the MaxSleep time, and on any expire that is going to expire
// after now but before MaxSleep.
// it's worth checking for stalled collaborators.
func (manager *Manager) computeNextTimeout(lastTick time.Time, leases map[lease.Key]lease.Info) {
	now := manager.config.Clock.Now()
	nextTick := now.Add(manager.config.MaxSleep)
	for _, info := range leases {
		if !info.Expiry.After(lastTick) {
			// The previous expire will expire this lease eventually, or
			// the manager will die with an error. Either way, we
			// don't need to worry about expiries in a previous expire
			// here.
			continue
		}
		if info.Expiry.After(nextTick) {
			continue
		}
		nextTick = info.Expiry
	}
	manager.config.Logger.Tracef("[%s] next expire in %v %v",
		manager.logContext, nextTick.Sub(now).Round(time.Millisecond), nextTick)
	manager.setNextTimeout(nextTick)
}

func (manager *Manager) setupInitialTimer() {
	// lastTick has never happened, so pass in the epoch time
	manager.computeNextTimeout(time.Time{}, manager.config.Store.Leases())
}

func (manager *Manager) setNextTimeout(t time.Time) {
	manager.muNextTimeout.Lock()
	manager.nextTimeout = t
	d := t.Sub(manager.config.Clock.Now())
	if manager.timer == nil {
		manager.timer = manager.config.Clock.NewTimer(d)
	} else {
		// See the docs on Timer.Reset() that says it isn't safe to call
		// on a non-stopped channel, and if it is stopped, you need to check
		// if the channel needs to be drained anyway. It isn't safe to drain
		// unconditionally in case another goroutine has already noticed,
		// but make an attempt.
		if !manager.timer.Stop() {
			select {
			case <-manager.timer.Chan():
			default:
			}
		}
		manager.timer.Reset(d)
	}
	manager.muNextTimeout.Unlock()
}

// ensureNextTimeout makes sure that the next timeout happens no-later than
// duration from now.
func (manager *Manager) ensureNextTimeout(d time.Duration) {
	manager.muNextTimeout.Lock()
	next := manager.nextTimeout
	proposed := manager.config.Clock.Now().Add(d)
	if next.After(proposed) {
		manager.config.Logger.Tracef("[%s] ensuring we wake up before %v at %v\n",
			manager.logContext, d, proposed)
		manager.nextTimeout = proposed
		manager.timer.Reset(d)
	}
	manager.muNextTimeout.Unlock()
}

func (manager *Manager) startRetry() *retry.Attempt {
	return retry.StartWithCancel(
		retry.LimitCount(maxRetries, retry.Exponential{
			Initial: initialRetryDelay,
			Factor:  retryBackoffFactor,
			Jitter:  true,
		}),
		manager.config.Clock,
		manager.catacomb.Dying(),
	)
}

func (manager *Manager) handlePin(p pin) {
	p.respond(errors.Trace(manager.config.Store.PinLease(p.leaseKey, p.entity, manager.catacomb.Dying())))
}

func (manager *Manager) handleUnpin(p pin) {
	p.respond(errors.Trace(manager.config.Store.UnpinLease(p.leaseKey, p.entity, manager.catacomb.Dying())))
}

// pinned returns lease names and the entities requiring their pinned
// behaviour, from the input namespace/model for which leases are pinned.
func (manager *Manager) pinned(namespace, modelUUID string) map[string][]string {
	pinned := make(map[string][]string)
	for key, entities := range manager.config.Store.Pinned() {
		if key.Namespace == namespace && key.ModelUUID == modelUUID {
			pinned[key.Lease] = entities
		}
	}
	return pinned
}

func (manager *Manager) leases(namespace, modelUUID string) map[string]string {
	leases := make(map[string]string)
	for key, lease := range manager.config.Store.LeaseGroup(namespace, modelUUID) {
		leases[key.Lease] = lease.Holder
	}
	return leases
}

func keysLess(a, b lease.Key) bool {
	if a.Namespace == b.Namespace && a.ModelUUID == b.ModelUUID {
		return a.Lease < b.Lease
	}
	if a.Namespace == b.Namespace {
		return a.ModelUUID < b.ModelUUID
	}
	return a.Namespace < b.Namespace
}

func (manager *Manager) startingClaim() {
	atomic.AddInt64(&manager.outstandingClaims, 1)
	manager.wg.Add(1)
}

func (manager *Manager) finishedClaim() {
	manager.wg.Done()
	atomic.AddInt64(&manager.outstandingClaims, -1)
}

func (manager *Manager) startingRevoke() {
	atomic.AddInt64(&manager.outstandingRevokes, 1)
	manager.wg.Add(1)
}

func (manager *Manager) finishedRevoke() {
	manager.wg.Done()
	atomic.AddInt64(&manager.outstandingRevokes, -1)
}

// Report is part of dependency.Reporter
func (manager *Manager) Report() map[string]interface{} {
	out := make(map[string]interface{})
	out["entity-uuid"] = manager.config.EntityUUID
	out["outstanding-claims"] = atomic.LoadInt64(&manager.outstandingClaims)
	out["outstanding-revokes"] = atomic.LoadInt64(&manager.outstandingRevokes)
	return out
}

func (manager *Manager) waitForGoroutines() {
	// Wait for the waitgroup to finish, but only up to a point.
	groupDone := make(chan struct{})
	go func() {
		manager.wg.Wait()
		close(groupDone)
	}()

	select {
	case <-groupDone:
		return
	case <-manager.config.Clock.After(maxShutdownWait):
	}
	msg := "timeout waiting for lease manager shutdown"
	dumpFile, err := manager.dumpDebug()
	logger := manager.config.Logger
	if err == nil {
		logger.Warningf("%v\ndebug info written to %v", msg, dumpFile)
	} else {
		logger.Warningf("%v\nerror writing debug info: %v", msg, err)
	}

}

func (manager *Manager) dumpDebug() (string, error) {
	dumpFile, err := os.OpenFile(filepath.Join(manager.config.LogDir, "lease-manager-debug.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return "", errors.Trace(err)
	}
	defer dumpFile.Close()
	claims := atomic.LoadInt64(&manager.outstandingClaims)
	revokes := atomic.LoadInt64(&manager.outstandingRevokes)
	template := `
lease manager state dump %v
entity-uuid: %v
outstanding-claims: %v
outstanding-revokes: %v

`[1:]
	message := fmt.Sprintf(template,
		time.Now().Format(time.RFC3339),
		manager.config.EntityUUID,
		claims,
		revokes,
	)
	if _, err = io.WriteString(dumpFile, message); err != nil {
		return "", errors.Annotate(err, "writing state to debug log file")
	}
	// Including the goroutines because the httpserver won't dump them
	// anymore if this worker stops happily.
	return dumpFile.Name(), pprof.Lookup("goroutine").WriteTo(dumpFile, 1)
}
