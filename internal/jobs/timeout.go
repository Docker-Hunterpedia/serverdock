package jobs

import "fmt"

// Strategies and the setting ranges (the services table documents the same ones).
const (
	StrategyBlueGreen = "blue_green"
	StrategyStopFirst = "stop_first"
)

// Settings are the snapshot values that decide a deploy job's timeout.
type Settings struct {
	Strategy       string
	HealthTimeoutS int // 5..600
	DrainS         int // 10..300
	StopGraceS     int // 0..300
}

// Validate checks the ranges.
func (s Settings) Validate() error {
	switch {
	case s.Strategy != StrategyBlueGreen && s.Strategy != StrategyStopFirst:
		return invalid("strategy must be blue_green or stop_first")
	case s.HealthTimeoutS < 5 || s.HealthTimeoutS > 600:
		return invalid("health_timeout_s must be 5..600")
	case s.DrainS < 10 || s.DrainS > 300:
		return invalid("drain_s must be 10..300")
	case s.StopGraceS < 0 || s.StopGraceS > 300:
		return invalid("stop_grace_s must be 0..300")
	}
	return nil
}

// The phase bounds of plan 0004 "Deadlines and contexts", in seconds.
const (
	preSwitchS      = 600 // pull + check + start
	switchS         = 30
	commitS         = 10
	stopRemoveS     = 30 // the remove after the stop
	stopExtraS      = 15 // added to stop_grace_s for the stop
	failureCleanupS = 60 // a re-apply and a candidate removal, 30 s each
	restartExtraS   = 30 // added to health_timeout_s for "restart the old container"
)

// TimeoutSeconds derives jobs.timeout_s at enqueue: the sum of every phase bound
// for the strategy, failure paths included, so no allowed setting can exceed it.
// Blue-green at the maximum settings is 600 + 600 + 30 + 10 + 300 + 345 + 60 =
// 1,945 s; stop-first adds the first stop (stop_grace_s + 15 s) and the restart
// path (health_timeout_s + 30 s), at most 945 s more. The watchdog itself is
// enforced by the runner, and only before the switch.
func TimeoutSeconds(s Settings) (int, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	total := preSwitchS + s.HealthTimeoutS + switchS + commitS + s.DrainS +
		(s.StopGraceS + stopExtraS + stopRemoveS) + failureCleanupS
	if s.Strategy == StrategyStopFirst {
		total += (s.StopGraceS + stopExtraS) + (s.HealthTimeoutS + restartExtraS)
	}
	return total, nil
}

// String is for logs and tests.
func (s Settings) String() string {
	return fmt.Sprintf("%s health=%ds drain=%ds grace=%ds", s.Strategy, s.HealthTimeoutS, s.DrainS, s.StopGraceS)
}

// RestartTimeoutSeconds derives jobs.timeout_s for a restart: the stop of the
// container (stop_grace_s + 15 s) and its health gate (health_timeout_s + 30 s).
// It is a recorded budget, NOT enforced as a watchdog: the restart's steps each
// run under their own bound (plan 0004, "Deadlines and contexts").
func RestartTimeoutSeconds(stopGraceS, healthTimeoutS int) int {
	return (stopGraceS + stopExtraS) + (healthTimeoutS + restartExtraS)
}

// DeleteTimeoutSeconds is jobs.timeout_s of a service delete: a generous bound for
// the stop and removal of a few containers (each stop_grace_s + 15 s, the removal
// 30 s) plus the rows. A recorded budget, NOT enforced as a watchdog: every Docker
// and route call of a delete runs under its own bound.
func DeleteTimeoutSeconds(stopGraceS int) int { return 120 + 3*(stopGraceS+stopExtraS+stopRemoveS) }
