// Copyright 2016, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package throttler

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/youtube/vitess/go/sync2"
	"github.com/youtube/vitess/go/vt/discovery"
	"github.com/youtube/vitess/go/vt/proto/throttlerdata"

	log "github.com/golang/glog"
	"github.com/golang/protobuf/proto"
)

type state int

const (
	increaseRate state = iota
	decreaseAndGuessRate
	emergency
)

type replicationLagChange int

const (
	unknown replicationLagChange = iota
	less
	equal
	greater
)

// memoryGranularity is the granularity at which values are rounded down in the
// memory instance. This is done to bucket similar rates (see also memory.go).
const memoryGranularity = 5

// MaxReplicationLagModule calculates the maximum rate based on observed
// replication lag and throttler rate changes.
// It implements the Module interface.
// Unless specified, the fields below are not guarded by a Mutex because they
// are only accessed within the Go routine running ProcessRecords().
type MaxReplicationLagModule struct {
	// config holds all parameters for this module.
	// It is only accessed by the Go routine which runs ProcessRecords() and does
	// not require locking.
	config MaxReplicationLagModuleConfig

	// initialMaxReplicationLagSec is the initial value of
	// config.MaxReplicationLagSec with which the module was started. We remember
	// it in case we need to reset the configuration.
	initialMaxReplicationLagSec int64

	// mutableConfigMu guards all fields in the group below.
	mutableConfigMu sync.Mutex
	// mutableConfig is the mutable copy of "config" which is currently used by
	// the module. By modifying "mutableConfig" and setting "applyMutableConfig"
	// to true, the next ProcessRecords() execution will copy "mutableConfig"
	// to "config" and become effective in the module.
	mutableConfig MaxReplicationLagModuleConfig
	// applyMutableConfig is set to true when ProcessRecords() should copy over
	// "mutableConfig" to "config".
	applyMutableConfig bool

	// rate is the rate calculated for the throttler.
	rate         sync2.AtomicInt64
	currentState state
	// lastRateChange is the time when rate was adjusted last.
	lastRateChange       time.Time
	lastRateChangeReason string
	nextAllowedIncrease  time.Time
	// replicaUnderIncreaseTest holds the discovery.TabletStats.Key value for the
	// replica for which we started the last increase test. After
	// nextAllowedIncrease is up, we wait for a lag update from this replica.
	replicaUnderIncreaseTest string
	nextAllowedDecrease      time.Time

	actualRatesHistory *aggregatedIntervalHistory
	lagCache           *replicationLagCache
	// memory tracks known good and bad throttler rates.
	memory *memory

	// rateUpdateChan is the notification channel to tell the throttler when our
	// max rate calculation has changed. The field is immutable (set in Start().)
	rateUpdateChan chan<- struct{}
	nowFunc        func() time.Time

	// lagRecords buffers the replication lag records received by the HealthCheck
	// listener. ProcessRecords() will process them.
	lagRecords chan replicationLagRecord
	wg         sync.WaitGroup
}

// NewMaxReplicationLagModule will create a new module instance and set the
// initial max replication lag limit to maxReplicationLag.
func NewMaxReplicationLagModule(config MaxReplicationLagModuleConfig, actualRatesHistory *aggregatedIntervalHistory, nowFunc func() time.Time) (*MaxReplicationLagModule, error) {
	if err := config.Verify(); err != nil {
		return nil, fmt.Errorf("invalid NewMaxReplicationLagModuleConfig: %v", err)
	}
	rate := int64(ReplicationLagModuleDisabled)
	if config.MaxReplicationLagSec != ReplicationLagModuleDisabled {
		rate = config.InitialRate
	}

	m := &MaxReplicationLagModule{
		initialMaxReplicationLagSec: config.MaxReplicationLagSec,
		// Register "config" for a future config update.
		mutableConfig:      config,
		applyMutableConfig: true,
		// Always start off with a non-zero rate because zero means all requests
		// get throttled.
		rate:       sync2.NewAtomicInt64(rate),
		memory:     newMemory(memoryGranularity),
		nowFunc:    nowFunc,
		lagRecords: make(chan replicationLagRecord, 10),
		// Prevent an immediately increase of the initial rate.
		nextAllowedIncrease: nowFunc().Add(config.MaxDurationBetweenIncreases()),
		actualRatesHistory:  actualRatesHistory,
		lagCache:            newReplicationLagCache(1000),
	}

	// Enforce a config update.
	m.applyLatestConfig()

	return m, nil
}

// Start launches a Go routine which reacts on replication lag records.
// It implements the Module interface.
func (m *MaxReplicationLagModule) Start(rateUpdateChan chan<- struct{}) {
	m.rateUpdateChan = rateUpdateChan
	m.wg.Add(1)
	go m.ProcessRecords()
}

// Stop blocks until the module's Go routine is stopped.
// It implements the Module interface.
func (m *MaxReplicationLagModule) Stop() {
	close(m.lagRecords)
	m.wg.Wait()
}

// MaxRate returns the current maximum allowed rate.
// It implements the Module interface.
func (m *MaxReplicationLagModule) MaxRate() int64 {
	return m.rate.Get()
}

// applyLatestConfig checks if "mutableConfig" should be applied as the new
// "config" and does so when necessary.
func (m *MaxReplicationLagModule) applyLatestConfig() {
	var config MaxReplicationLagModuleConfig
	applyConfig := false

	m.mutableConfigMu.Lock()
	if m.applyMutableConfig {
		config = m.mutableConfig
		applyConfig = true
		m.applyMutableConfig = false
	}
	m.mutableConfigMu.Unlock()

	// No locking required here because this method is only called from the same
	// Go routine as ProcessRecords() or the constructor.
	if applyConfig {
		m.config = config
	}
}

func (m *MaxReplicationLagModule) getConfiguration() *throttlerdata.Configuration {
	m.mutableConfigMu.Lock()
	defer m.mutableConfigMu.Unlock()

	configCopy := m.mutableConfig.Configuration
	return &configCopy
}

func (m *MaxReplicationLagModule) updateConfiguration(configuration *throttlerdata.Configuration, copyZeroValues bool) error {
	m.mutableConfigMu.Lock()
	defer m.mutableConfigMu.Unlock()

	newConfig := m.mutableConfig

	if copyZeroValues {
		newConfig.Configuration = *proto.Clone(configuration).(*throttlerdata.Configuration)
	} else {
		proto.Merge(&newConfig.Configuration, configuration)
	}

	if err := newConfig.Verify(); err != nil {
		return err
	}
	m.mutableConfig = newConfig
	return nil
}

func (m *MaxReplicationLagModule) resetConfiguration() {
	m.mutableConfigMu.Lock()
	defer m.mutableConfigMu.Unlock()

	m.mutableConfig = NewMaxReplicationLagModuleConfig(m.initialMaxReplicationLagSec)
}

// RecordReplicationLag records the current replication lag for processing.
func (m *MaxReplicationLagModule) RecordReplicationLag(t time.Time, ts *discovery.TabletStats) {
	m.mutableConfigMu.Lock()
	if m.mutableConfig.MaxReplicationLagSec == ReplicationLagModuleDisabled {
		m.mutableConfigMu.Unlock()
		return
	}
	m.mutableConfigMu.Unlock()

	// Buffer data point for now to unblock the HealthCheck listener and process
	// it asynchronously in ProcessRecords().
	m.lagRecords <- replicationLagRecord{t, *ts}
}

// ProcessRecords is the main loop, run in a separate Go routine, which
// reacts to any replication lag updates (recordings).
func (m *MaxReplicationLagModule) ProcessRecords() {
	defer m.wg.Done()

	for lagRecord := range m.lagRecords {
		m.processRecord(lagRecord)
	}
}

func (m *MaxReplicationLagModule) processRecord(lagRecord replicationLagRecord) {
	m.applyLatestConfig()

	m.lagCache.add(lagRecord)

	m.lagCache.sortByLag(int(m.config.IgnoreNSlowestReplicas), m.config.MaxReplicationLagSec+1)

	if m.lagCache.ignoreSlowReplica(lagRecord.Key) {
		return
	}

	m.recalculateRate(lagRecord)
}

func (m *MaxReplicationLagModule) recalculateRate(lagRecordNow replicationLagRecord) {
	now := m.nowFunc()

	if lagRecordNow.isZero() {
		panic("rate recalculation was triggered with a zero replication lag record")
	}
	lagNow := lagRecordNow.lag()

	if lagNow <= m.config.TargetReplicationLagSec {
		// Lag in range: [0, target]
		m.increaseRate(now, lagRecordNow)
	} else if lagNow <= m.config.MaxReplicationLagSec {
		// Lag in range: (target, max]
		m.decreaseAndGuessRate(now, lagRecordNow)
	} else {
		// Lag in range: (max, infinite]
		m.emergency(now, lagRecordNow)
	}
}

func (m *MaxReplicationLagModule) increaseRate(now time.Time, lagRecordNow replicationLagRecord) {
	// Any increase has to wait for a previous decrease first.
	if !m.nextAllowedDecrease.IsZero() && now.Before(m.nextAllowedDecrease) {
		return
	}
	// Increase rate again only if the last increase was in effect long enough.
	if !m.nextAllowedIncrease.IsZero() && now.Before(m.nextAllowedIncrease) {
		return
	}

	// We wait for a lag record for the same replica and ignore other replica
	// updates in the meantime.
	if m.replicaUnderIncreaseTest != "" && m.replicaUnderIncreaseTest != lagRecordNow.Key {
		// This is not the replica we're waiting for. Therefore we'll skip this
		// replica if the replica under test has no issues i.e.
		// a) it's still tracked
		// b) its LastError is not set
		// c) it has not become a slow, ignored replica
		r := m.lagCache.latest(m.replicaUnderIncreaseTest)
		if !r.isZero() && r.LastError == nil && !m.lagCache.isIgnored(m.replicaUnderIncreaseTest) {
			return
		}
	}

	oldRate := m.rate.Get()

	m.markCurrentRateAsBadOrGood(now, increaseRate, unknown)
	m.resetCurrentState(now)

	// Calculate new rate based on the previous (preferrably actual) rate.
	highestGood := m.memory.highestGood()
	previousRateSource := "highest known good rate"
	previousRate := float64(highestGood)
	if previousRate == 0.0 && !m.lastRateChange.IsZero() {
		// No known high good rate. Use the actual value instead.
		// (It might be lower because the system was slower or the throttler rate was
		// set by a different module and not us.)
		previousRateSource = "previous actual rate"
		previousRate = m.actualRatesHistory.average(m.lastRateChange, now)
	}
	if previousRate == 0.0 || math.IsNaN(previousRate) {
		// NaN (0.0/0.0) occurs when no observations were in the timespan.
		// Use the set rate in this case.
		previousRateSource = "previous set rate"
		previousRate = float64(oldRate)
	}

	// a) Increase rate by MaxIncrease.
	increaseReason := fmt.Sprintf("a max increase of %.1f%%", m.config.MaxIncrease*100)
	rate := previousRate * (1 + m.config.MaxIncrease)

	// b) Always make minimum progress compared to oldRate.
	// (Necessary for cases where MaxIncrease is too low and the rate might not increase.)
	if rate <= float64(oldRate) {
		rate = float64(oldRate) + memoryGranularity
		increaseReason += fmt.Sprintf(" (minimum progress by %v)", memoryGranularity)
		previousRateSource = "previous set rate"
		previousRate = float64(oldRate)
	}
	// c) Make the increase less aggressive if it goes above the bad rate.
	lowestBad := float64(m.memory.lowestBad())
	if lowestBad != 0 {
		if rate > lowestBad {
			// New rate will be the middle value of [previous rate, lowest bad rate].
			rate -= (lowestBad - previousRate) / 2
			increaseReason += fmt.Sprintf(" (but limited to the middle value in the range [previous rate, lowest bad rate]: [%.0f, %.0f]", previousRate, lowestBad)
		}
	}

	increase := (rate - previousRate) / previousRate
	m.updateNextAllowedIncrease(now, increase, lagRecordNow.Key)
	reason := fmt.Sprintf("periodic increase of the %v from %d to %d (by %.1f%%) based on %v to find out the maximum - next allowed increase in %.0f seconds",
		previousRateSource, oldRate, int64(rate), increase*100, increaseReason, m.nextAllowedIncrease.Sub(now).Seconds())
	m.updateRate(increaseRate, int64(rate), reason, now, lagRecordNow)
}

func (m *MaxReplicationLagModule) updateNextAllowedIncrease(now time.Time, increase float64, key string) {
	minDuration := m.config.MinDurationBetweenChanges()
	// We may have to wait longer than the configured minimum duration
	// until we see an effect of the increase.
	// Example: If the increase was fully over the capacity, it will take
	// 1 / increase seconds until the replication lag goes up by 1 second.
	// E.g.
	// (If the system was already at its maximum capacity (e.g. 1k QPS) and we
	// increase the rate by e.g. 5% to 1050 QPS, it will take 20 seconds until
	// 1000 extra queries are buffered and the lag increases by 1 second.)
	// On top of that, add 2 extra seconds to account for a delayed propagation
	// of the data (because the throttler takes over the updated rate only every
	// second and it publishes its rate history only after a second is over).
	// TODO(mberlin): Instead of adding 2 seconds, should we wait for twice the
	// calculated time instead?
	minPropagationTime := time.Duration(1.0/increase+2) * time.Second
	if minPropagationTime > minDuration {
		minDuration = minPropagationTime
	}
	if minDuration > m.config.MaxDurationBetweenIncreases() {
		// Cap the rate to a reasonable amount of time (very small increases may
		// result into a 20 minutes wait otherwise.)
		minDuration = m.config.MaxDurationBetweenIncreases()
	}
	m.nextAllowedIncrease = now.Add(minDuration)
	m.replicaUnderIncreaseTest = key
}

func (m *MaxReplicationLagModule) decreaseAndGuessRate(now time.Time, lagRecordNow replicationLagRecord) {
	// Decrease the rate only if the last decrease was in effect long enough.
	if !m.nextAllowedDecrease.IsZero() && now.Before(m.nextAllowedDecrease) {
		return
	}

	// Guess slave rate based on the difference in the replication lag of this
	// particular replica.
	lagRecordBefore := m.lagCache.atOrAfter(lagRecordNow.Key, m.lastRateChange)
	if lagRecordBefore.isZero() {
		// We don't know the replication lag of this replica since the last rate
		// change. Without it we won't be able to guess the slave rate.
		// Therefore, we'll stay in the current state and wait for more records.
		return
	}

	if lagRecordBefore.time == lagRecordNow.time {
		// First record is the same as the last record. Not possible to calculate a
		// diff. Wait for the next health stats update.
		return
	}

	// Analyze if the past rate was good or bad.
	lagBefore := lagRecordBefore.lag()
	lagNow := lagRecordNow.lag()
	replicationLagChange := less
	// Note that we consider lag changes of 1 second as equal as well because
	// they might be a rounding error in MySQL due to using timestamps at
	// second granularity.
	if lagNow == lagBefore || math.Abs(float64(lagNow-lagBefore)) == 1 {
		replicationLagChange = equal
	} else if lagNow > lagBefore {
		replicationLagChange = greater
	}
	m.markCurrentRateAsBadOrGood(now, decreaseAndGuessRate, replicationLagChange)
	m.resetCurrentState(now)

	if replicationLagChange == equal {
		// The replication lag did not change. Keep going at the current rate.
		return
	}

	// Find out the average rate (per second) at which we inserted data
	// at the master during the observed timespan.
	from := lagRecordBefore.time
	to := lagRecordNow.time
	avgMasterRate := m.actualRatesHistory.average(from, to)
	if math.IsNaN(avgMasterRate) {
		// NaN (0.0/0.0) occurs when no observations were in the timespan.
		// Wait for more rate observations.
		return
	}

	// Sanity check and correct the data points if necessary.
	d := lagRecordNow.time.Sub(lagRecordBefore.time)
	lagDifference := time.Duration(lagRecordNow.lag()-lagRecordBefore.lag()) * time.Second
	if lagDifference > d {
		log.Errorf("Replication lag increase is higher than the elapsed time: %v > %v. This should not happen. Replication Lag Data points: Before: %+v Now: %+v", lagDifference, d, lagRecordBefore, lagRecordNow)
		d = lagDifference
	}

	// Guess the slave capacity based on the replication lag change.
	rate := m.guessSlaveRate(avgMasterRate, lagBefore, lagNow, lagDifference, d)

	m.nextAllowedDecrease = now.Add(m.config.MinDurationBetweenChanges() + 2*time.Second)
	reason := "reaction to replication lag change"
	m.updateRate(decreaseAndGuessRate, rate, reason, now, lagRecordNow)
}

func (m *MaxReplicationLagModule) guessSlaveRate(avgMasterRate float64, lagBefore, lagNow int64, lagDifference, d time.Duration) int64 {
	// avgSlaveRate is the average rate (per second) at which the slave
	// applied transactions from the replication stream. We infer the value
	// from the relative change in the replication lag.
	avgSlaveRate := avgMasterRate * (d - lagDifference).Seconds() / d.Seconds()
	log.Infof("d : %v lag diff: %v master: %v slave: %v", d, lagDifference, avgMasterRate, avgSlaveRate)

	oldRequestsBehind := 0.0
	// If the old lag was > 0s, the slave needs to catch up on that as well.
	if lagNow > lagBefore {
		oldRequestsBehind = avgSlaveRate * float64(lagBefore)
	}
	newRequestsBehind := 0.0
	// If the lag increased (i.e. slave rate was slower), the slave must make up
	// for the difference in the future.
	if avgSlaveRate < avgMasterRate {
		newRequestsBehind = (avgMasterRate - avgSlaveRate) * d.Seconds()
	}
	requestsBehind := oldRequestsBehind + newRequestsBehind
	log.Infof("old reqs: %v new reqs: %v total: %v", oldRequestsBehind, newRequestsBehind, requestsBehind)

	// Reduce the new rate such that it has time to catch up the requests it's
	// behind within the next interval.
	futureRequests := avgSlaveRate * m.config.MinDurationBetweenChanges().Seconds()
	if futureRequests > 0 {
		avgSlaveRate *= (futureRequests - requestsBehind) / futureRequests
		if avgSlaveRate < 1 {
			// Backlog is too high. Reduce rate to 1 request/second.
			avgSlaveRate = 1.0
		}
		log.Infof("slave after future reqs adj: %v", avgSlaveRate)
	}

	return int64(avgSlaveRate)
}

func (m *MaxReplicationLagModule) emergency(now time.Time, lagRecordNow replicationLagRecord) {
	m.markCurrentRateAsBadOrGood(now, emergency, unknown)
	m.resetCurrentState(now)

	oldRate := m.rate.Get()
	rate := int64(float64(oldRate) * m.config.EmergencyDecrease)

	reason := fmt.Sprintf("replication lag went beyond max: %d > %d reducing previous rate by %.f%% to: %v", lagRecordNow.lag(), m.config.MaxReplicationLagSec, m.config.EmergencyDecrease*100, rate)
	m.updateRate(emergency, rate, reason, now, lagRecordNow)
}

func (m *MaxReplicationLagModule) updateRate(newState state, rate int64, reason string, now time.Time, lagRecordNow replicationLagRecord) {
	oldRate := m.rate.Get()

	m.currentState = newState
	m.lastRateChange = now
	m.lastRateChangeReason = reason

	if rate == oldRate {
		return
	}

	log.Infof("updated rate from: %v to: %v reason: %v", oldRate, rate, m.lastRateChangeReason)
	m.rate.Set(int64(rate))
	// Notify the throttler that we updated our max rate.
	m.rateUpdateChan <- struct{}{}
}

// markCurrentRateAsBadOrGood determines the actual rate between the last rate
// change and "now" and determines if that rate was bad or good.
func (m *MaxReplicationLagModule) markCurrentRateAsBadOrGood(now time.Time, newState state, replicationLagChange replicationLagChange) {
	if m.lastRateChange.IsZero() {
		// Module was just started. We don't have any data points yet.
		return
	}

	// Use the actual rate instead of the set rate.
	// (It might be lower because the system was slower or the throttler rate was
	// set by a different module and not us.)
	rate := m.actualRatesHistory.average(m.lastRateChange, now)
	if math.IsNaN(rate) {
		// NaN (0.0/0.0) occurs when no observations were in the timespan.
		// Wait for more observations.
		return
	}

	rateIsGood := false

	switch m.currentState {
	case increaseRate:
		switch newState {
		case increaseRate:
			rateIsGood = true
		case decreaseAndGuessRate:
			rateIsGood = false
		case emergency:
			rateIsGood = false
		}
	case decreaseAndGuessRate:
		switch newState {
		case increaseRate:
			rateIsGood = true
		case decreaseAndGuessRate:
			switch replicationLagChange {
			case unknown:
				return
			case less:
				rateIsGood = true
			case equal:
				// Replication lag kept constant. Impossible to judge if the rate is good or bad.
				return
			case greater:
				rateIsGood = false
			}
		case emergency:
			rateIsGood = false
		}
	case emergency:
		// Rate changes initiated during an "emergency" phase provide no meaningful data point.
		return
	}

	if rateIsGood {
		log.Infof("marking rate %.f as good state: %v", rate, m.currentState)
		m.memory.markGood(int64(rate))
	} else {
		log.Infof("marking rate %.f as bad state: %v", rate, m.currentState)
		m.memory.markBad(int64(rate))
	}
}

// resetCurrentState ensures that any state set in a previous iteration is
// reset.
func (m *MaxReplicationLagModule) resetCurrentState(now time.Time) {
	switch m.currentState {
	case increaseRate:
		m.nextAllowedIncrease = now
	case decreaseAndGuessRate:
		m.nextAllowedDecrease = now
	}
}
