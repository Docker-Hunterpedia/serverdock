package deploy

// The stages of a deploy (plan 0004, "Blue-green deploy"), as deploy_stages.stage
// and deploys.stage name them. Step 8a runs the first four; switch, commit, drain
// and stop_old are step 8b (switch.go); StageDone is deploys.stage of a finished
// (succeeded) deploy.
const (
	StagePull    = "pull"
	StageCheck   = "check"
	StageStart   = "start"
	StageHealth  = "health"
	StageSwitch  = "switch"
	StageCommit  = "commit"
	StageDrain   = "drain"
	StageStopOld = "stop_old"
	StageDone    = "done"
)

// blueGreenStages is the ordered stage list written (as pending) when a deploy
// starts to run. Stop-first adds its own stages in step 9.
var blueGreenStages = []string{StagePull, StageCheck, StageStart, StageHealth, StageSwitch, StageCommit, StageDrain, StageStopOld}

// Stage states (deploy_stages.state).
const (
	StagePending = "pending"
	StageRunning = "running"
	StageOK      = "ok"
	StageFailed  = "failed"
	StageSkipped = "skipped"
)

// CancellableStage reports whether a cancel, a supersede, a session revoke or a
// shutdown takes effect in stage s. Switch and commit are not cancellable (they
// run on a context cut off from the job's), and nothing after the commit is: the
// new version is live by then.
func CancellableStage(s string) bool {
	switch s {
	case StageSwitch, StageCommit, StageDrain, StageStopOld, StageDone:
		return false
	}
	return true
}
