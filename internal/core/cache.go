package core

import (
	"encoding/json"
	"errors"
	"time"
)

// cacheEntry is one account's timing state and last reading. Two keys are special:
// "$machine" (the last request from anywhere) and "$profile" (the identity of Claude
// Code's current token). Times are Unix milliseconds.
type cacheEntry struct {
	Usage         json.RawMessage `json:"usage,omitempty"`
	FetchedAt     int64           `json:"fetchedAt,omitempty"`
	AttemptedAt   int64           `json:"attemptedAt,omitempty"`
	LimitedUntil  int64           `json:"limitedUntil,omitempty"`
	Strikes       int             `json:"strikes,omitempty"`
	LastLimitedAt int64           `json:"lastLimitedAt,omitempty"`
	LastError     string          `json:"lastError,omitempty"`

	Fingerprint string `json:"fingerprint,omitempty"`
	Email       string `json:"email,omitempty"`
	Org         string `json:"org,omitempty"`
	VerifiedAt  int64  `json:"verifiedAt,omitempty"`
	FailedAt    int64  `json:"failedAt,omitempty"`
}

const (
	machineKey = "$machine"
	profileKey = "$profile"
)

var errLocked = errors.New("the usage cache is locked by another process")

type cache map[string]*cacheEntry

func (c *Config) readCache() cache {
	out := cache{}
	if _, err := readJSONFile(c.cacheFile(), &out); err != nil {
		return cache{}
	}
	return out
}

func (c *Config) writeCache(data cache) {
	_ = writePrivateJSON(c.cacheFile(), data) // best effort
}

// withCacheLock runs fn holding an exclusive flock on the lock file. Every front end
// on the machine shares the cache, and an unlocked read-modify-write from two
// processes loses whichever update landed first — including a 429 cooldown.
func (c *Config) withCacheLock(fn func(cache) bool) error {
	err := withFileLock(c.lockFile(), func() error {
		data := c.readCache()
		if fn(data) {
			c.writeCache(data)
		}
		return nil
	})
	if errors.Is(err, errLockTimeout) {
		return errLocked
	}
	return err
}

// cacheUpdate merges into (or with replace, replaces) one entry under the lock.
func (c *Config) cacheUpdate(key string, mutate func(e *cacheEntry) *cacheEntry) {
	err := c.withCacheLock(func(data cache) bool {
		e := data[key]
		if e == nil {
			e = &cacheEntry{}
		}
		data[key] = mutate(e)
		return true
	})
	if err != nil {
		c.Warn("could not update the usage cache: " + err.Error())
	}
}

func (c *Config) cacheDelete(key string) {
	_ = c.withCacheLock(func(data cache) bool {
		if _, ok := data[key]; !ok {
			return false
		}
		delete(data, key)
		return true
	})
}

type gateKind int

const (
	gateGo gateKind = iota
	gateCooldown
	gateSpacing
	gateInflight
	gateWait
)

type gate struct {
	kind      gateKind
	entry     cacheEntry
	wait      time.Duration
	claimedAt int64
}

func (e *cacheEntry) lastAttempt() int64 { return max(e.AttemptedAt, e.FetchedAt) }

// claimFetchSlot decides, atomically across processes, whether a usage request for
// key may go out now. A granted slot is recorded before the request is sent, so even
// a crash mid-request counts against the spacing.
//
// renewedAt is when the caller's token record was last written: a login the last
// attempt found dead and that has been renewed since is a different token, and does
// not wait out the spacing behind the failure.
func (c *Config) claimFetchSlot(key string, renewedAt int64) (gate, error) {
	var g gate
	err := c.withCacheLock(func(data cache) bool {
		now := c.now().UnixMilli()
		e := cacheEntry{}
		if data[key] != nil {
			e = *data[key]
		}
		g.entry = e
		if e.LimitedUntil > now {
			g.kind = gateCooldown
			return false
		}
		spacing := MinFetchSpacing.Milliseconds()
		if e.LastLimitedAt > 0 && now-e.LastLimitedAt < limitedMemory.Milliseconds() {
			spacing *= 2
		}
		since := now - e.lastAttempt()
		renewed := isDeadLoginError(e.LastError) && renewedAt > e.lastAttempt()
		if !renewed && since < spacing-spacingTolerance.Milliseconds() {
			if e.Usage == nil && e.LastError == "" && e.AttemptedAt > 0 && since < inflightWait.Milliseconds() {
				g.kind = gateInflight
			} else {
				g.kind = gateSpacing
			}
			return false
		}
		// The stagger is about real request bursts, so it runs on the wall clock.
		wall := time.Now().UnixMilli()
		var machineAt int64
		if m := data[machineKey]; m != nil {
			machineAt = m.AttemptedAt
		}
		if sinceAny := wall - machineAt; sinceAny >= 0 && sinceAny < accountStagger.Milliseconds() {
			g.kind, g.wait = gateWait, time.Duration(accountStagger.Milliseconds()-sinceAny)*time.Millisecond
			return false
		}
		e.AttemptedAt = now
		data[key] = &e
		data[machineKey] = &cacheEntry{AttemptedAt: wall}
		g.kind, g.claimedAt = gateGo, now
		return true
	})
	return g, err
}
