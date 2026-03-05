package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nullne/star-fleet/internal/agent"
	"github.com/nullne/star-fleet/internal/config"
	"github.com/nullne/star-fleet/internal/gh"
	"github.com/nullne/star-fleet/internal/git"
	"github.com/nullne/star-fleet/internal/review"
	"github.com/nullne/star-fleet/internal/state"
	"github.com/nullne/star-fleet/internal/ui"
	"github.com/nullne/star-fleet/internal/validate"
)

type Orchestrator struct {
	Owner    string
	Repo     string
	Number   int
	Config   *config.Config
	Display  *ui.Display
	RepoRoot string
	Restart  bool // when true, discard existing state and start fresh
}

func (o *Orchestrator) Run(ctx context.Context) error {
	s, err := o.loadState()
	if err != nil {
		return err
	}

	if s.Phase == state.PhaseDone {
		o.Display.Success("Pipeline already completed for this issue")
		return nil
	}

	resuming := s.Phase != state.PhaseNew

	if s.BaseBranch == "" {
		baseBranch, err := gh.DefaultBranch(ctx, o.Owner, o.Repo)
		if err != nil {
			return fmt.Errorf("detecting default branch: %w", err)
		}
		s.BaseBranch = baseBranch
	}

	o.Display.Title(o.Owner, o.Repo, o.Number)
	if resuming {
		o.Display.Info(fmt.Sprintf("⟳ Resuming from %s phase", s.Phase))
	}

	// === INTAKE ===
	issue, err := o.phaseIntake(ctx, s)
	if err != nil {
		return err
	}

	// === WORKTREES (idempotent) ===
	devDir, err := git.CreateWorktree(ctx, o.RepoRoot, "dev", s.DevBranch)
	if err != nil {
		o.Display.StepFail("Creating worktrees...", err.Error())
		return fmt.Errorf("creating dev worktree: %w", err)
	}
	testDir, err := git.CreateWorktree(ctx, o.RepoRoot, "test", s.TestBranch)
	if err != nil {
		o.Display.StepFail("Creating worktrees...", err.Error())
		return fmt.Errorf("creating test worktree: %w", err)
	}
	defer o.cleanup(ctx)

	backend, err := agent.NewBackend(o.Config.Agent.Backend)
	if err != nil {
		return err
	}

	devAgent := &agent.DevAgent{
		Backend: backend, Owner: o.Owner, Repo: o.Repo,
		Issue: issue, Workdir: devDir,
		Branch: s.DevBranch, BaseBranch: s.BaseBranch,
	}
	testAgent := &agent.TestAgent{
		Backend: backend, Owner: o.Owner, Repo: o.Repo,
		Issue: issue, Workdir: testDir,
		Branch: s.TestBranch, BaseBranch: s.BaseBranch,
	}

	// === DISPATCH (run agents) ===
	o.Display.Blank()
	if err := o.phaseDispatch(ctx, s, devAgent, testAgent); err != nil {
		return err
	}

	// === PUSH ===
	if err := o.phasePush(ctx, s, devAgent, testAgent); err != nil {
		return err
	}

	// === CREATE PRs ===
	devPR, testPR, err := o.phasePRs(ctx, s, devAgent, testAgent)
	if err != nil {
		return err
	}

	// === REVIEW ===
	o.Display.Blank()
	if err := o.phaseReview(ctx, s, backend, devAgent, testAgent, devPR, testPR); err != nil {
		return err
	}

	// === CROSS-VALIDATION + DELIVER ===
	return o.phaseValidate(ctx, s, devAgent, testAgent, issue)
}

// ---------------------------------------------------------------------------
// State management
// ---------------------------------------------------------------------------

func (o *Orchestrator) loadState() (*state.RunState, error) {
	if o.Restart {
		return state.New(o.RepoRoot, o.Owner, o.Repo, o.Number), nil
	}
	s, err := state.Load(o.RepoRoot, o.Number)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s, nil
	}
	return state.New(o.RepoRoot, o.Owner, o.Repo, o.Number), nil
}

// ---------------------------------------------------------------------------
// Phase: Intake
// ---------------------------------------------------------------------------

func (o *Orchestrator) phaseIntake(ctx context.Context, s *state.RunState) (*gh.Issue, error) {
	if s.Phase.AtLeast(state.PhaseIntake) {
		issue, err := gh.FetchIssue(ctx, o.Owner, o.Repo, o.Number)
		if err != nil {
			return nil, err
		}
		o.Display.Step("Fetching issue...", issue.Title)
		o.Display.Step("Validating spec...", "")
		return issue, nil
	}

	issue, err := o.intake(ctx)
	if err != nil {
		return nil, err
	}
	if err := o.postPickedUp(ctx); err != nil {
		return nil, err
	}

	s.IssueTitle = issue.Title
	if err := s.Advance(state.PhaseIntake); err != nil {
		return nil, err
	}
	return issue, nil
}

func (o *Orchestrator) intake(ctx context.Context) (*gh.Issue, error) {
	sp := o.Display.TreeLeaf("Fetching issue...", "")
	issue, err := gh.FetchIssue(ctx, o.Owner, o.Repo, o.Number)
	if err != nil {
		sp.Stop("fail", err.Error())
		return nil, err
	}
	sp.Stop("success", issue.Title)

	if err := o.validateSpec(ctx, issue); err != nil {
		return nil, err
	}
	o.Display.Step("Validating spec...", "")
	return issue, nil
}

func (o *Orchestrator) validateSpec(ctx context.Context, issue *gh.Issue) error {
	if strings.TrimSpace(issue.Body) == "" {
		gaps := "The issue body is empty. Please add a description of the desired behavior."
		comment := "## 🔍 Star Fleet — Spec Gap\n\n" + gaps + "\n\nPipeline paused. Please update the issue and re-run."
		_ = gh.PostComment(ctx, o.Owner, o.Repo, o.Number, comment)
		o.Display.StepFail("Validating spec...", "empty issue body")
		return fmt.Errorf("issue #%d has an empty body", o.Number)
	}
	if len(issue.Body) < 50 {
		gaps := "The issue description seems too brief. Please provide more detail about expected behavior, acceptance criteria, or edge cases."
		comment := "## 🔍 Star Fleet — Spec Gap\n\n" + gaps + "\n\nPipeline paused. Please update the issue and re-run."
		_ = gh.PostComment(ctx, o.Owner, o.Repo, o.Number, comment)
		o.Display.StepWarn("Validating spec...", "issue body is very short")
		return fmt.Errorf("issue #%d body is too brief (%d chars)", o.Number, len(issue.Body))
	}
	return nil
}

func (o *Orchestrator) postPickedUp(ctx context.Context) error {
	return gh.PostComment(ctx, o.Owner, o.Repo, o.Number,
		"## 🚀 Star Fleet — Picked Up\n\nThis issue has been picked up by Star Fleet. Dev and Test agents are being dispatched.")
}

// ---------------------------------------------------------------------------
// Phase: Dispatch (run agents, with per-agent checkpointing)
// ---------------------------------------------------------------------------

func (o *Orchestrator) phaseDispatch(ctx context.Context, s *state.RunState, devAgent *agent.DevAgent, testAgent *agent.TestAgent) error {
	if s.Phase.AtLeast(state.PhaseDispatch) {
		o.Display.Step("Dev Agent", "done (cached)")
		o.Display.Step("Test Agent", "done (cached)")
		return nil
	}

	lv := o.Display.StartLiveView([]ui.AgentConfig{
		{Label: "Dev Agent", Tree: "├", Message: "Writing implementation..."},
		{Label: "Test Agent", Tree: "└", Message: "Writing tests..."},
	})
	defer lv.Stop()

	devPanel := lv.Panel(0)
	testPanel := lv.Panel(1)

	type agentResult struct {
		role string
		err  error
	}

	toRun := 0
	if !s.DevAgentDone {
		toRun++
	}
	if !s.TestAgentDone {
		toRun++
	}

	results := make(chan agentResult, toRun)
	var wg sync.WaitGroup

	if s.DevAgentDone {
		devPanel.Finish("success", "done (cached)")
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := devAgent.Run(ctx, devPanel)
			if err == nil {
				s.DevAgentDone = true
				_ = s.Save()
			}
			results <- agentResult{role: "dev", err: err}
		}()
	}

	if s.TestAgentDone {
		testPanel.Finish("success", "done (cached)")
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := testAgent.Run(ctx, testPanel)
			if err == nil {
				s.TestAgentDone = true
				_ = s.Save()
			}
			results <- agentResult{role: "test", err: err}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var devErr, testErr error
	for r := range results {
		switch r.role {
		case "dev":
			if r.err != nil {
				devErr = r.err
				devPanel.Finish("fail", r.err.Error())
			} else {
				devPanel.Finish("success", "done")
			}
		case "test":
			if r.err != nil {
				testErr = r.err
				testPanel.Finish("fail", r.err.Error())
			} else {
				testPanel.Finish("success", "done")
			}
		}
	}

	if devErr != nil {
		return fmt.Errorf("dev agent: %w", devErr)
	}
	if testErr != nil {
		return fmt.Errorf("test agent: %w", testErr)
	}

	return s.Advance(state.PhaseDispatch)
}

// ---------------------------------------------------------------------------
// Phase: Push (idempotent — git push is a no-op if already up to date)
// ---------------------------------------------------------------------------

func (o *Orchestrator) phasePush(ctx context.Context, s *state.RunState, devAgent *agent.DevAgent, testAgent *agent.TestAgent) error {
	if s.Phase.AtLeast(state.PhasePush) {
		return nil
	}

	if err := git.Push(ctx, devAgent.Workdir, "origin", devAgent.Branch); err != nil {
		o.Display.StepFail("Pushing branches...", fmt.Sprintf("dev: %v", err))
		return fmt.Errorf("pushing dev branch: %w", err)
	}

	if err := git.Push(ctx, testAgent.Workdir, "origin", testAgent.Branch); err != nil {
		o.Display.StepFail("Pushing branches...", fmt.Sprintf("test: %v", err))
		return fmt.Errorf("pushing test branch: %w", err)
	}

	o.Display.Step("Pushing branches...", "")
	return s.Advance(state.PhasePush)
}

// ---------------------------------------------------------------------------
// Phase: PRs (idempotent — checks for existing PR before creating)
// ---------------------------------------------------------------------------

func (o *Orchestrator) phasePRs(ctx context.Context, s *state.RunState, devAgent *agent.DevAgent, testAgent *agent.TestAgent) (*gh.PR, *gh.PR, error) {
	if s.Phase.AtLeast(state.PhasePRs) {
		devPR := &gh.PR{Number: s.DevPR.Number, URL: s.DevPR.URL}
		testPR := &gh.PR{Number: s.TestPR.Number, URL: s.TestPR.URL}
		o.Display.Step(fmt.Sprintf("Dev PR #%d", devPR.Number), devPR.URL)
		o.Display.Step(fmt.Sprintf("Test PR #%d", testPR.Number), testPR.URL)
		return devPR, testPR, nil
	}

	devPR, err := o.findOrCreatePR(ctx, devAgent.Workdir,
		fmt.Sprintf("[fleet/dev] #%d %s", devAgent.Issue.Number, devAgent.Issue.Title),
		fmt.Sprintf("Implementation for #%d by Star Fleet Dev Agent.", devAgent.Issue.Number),
		s.BaseBranch, devAgent.Branch)
	if err != nil {
		o.Display.StepFail("Creating dev PR...", err.Error())
		return nil, nil, fmt.Errorf("creating dev PR: %w", err)
	}
	o.Display.Step(fmt.Sprintf("Dev PR #%d", devPR.Number), devPR.URL)

	testPR, err := o.findOrCreatePR(ctx, testAgent.Workdir,
		fmt.Sprintf("[fleet/test] #%d %s", testAgent.Issue.Number, testAgent.Issue.Title),
		fmt.Sprintf("Tests for #%d by Star Fleet Test Agent.", testAgent.Issue.Number),
		s.BaseBranch, testAgent.Branch)
	if err != nil {
		o.Display.StepFail("Creating test PR...", err.Error())
		return nil, nil, fmt.Errorf("creating test PR: %w", err)
	}
	o.Display.Step(fmt.Sprintf("Test PR #%d", testPR.Number), testPR.URL)

	s.DevPR = &state.PRInfo{Number: devPR.Number, URL: devPR.URL}
	s.TestPR = &state.PRInfo{Number: testPR.Number, URL: testPR.URL}
	if err := s.Advance(state.PhasePRs); err != nil {
		return nil, nil, err
	}
	return devPR, testPR, nil
}

func (o *Orchestrator) findOrCreatePR(ctx context.Context, workdir, title, body, base, head string) (*gh.PR, error) {
	existing, err := gh.FindPR(ctx, o.Owner, o.Repo, head)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	return gh.CreatePR(ctx, workdir, title, body, base, head)
}

// ---------------------------------------------------------------------------
// Phase: Review
// ---------------------------------------------------------------------------

func (o *Orchestrator) phaseReview(ctx context.Context, s *state.RunState, backend agent.Backend, devAgent *agent.DevAgent, testAgent *agent.TestAgent, devPR, testPR *gh.PR) error {
	if s.Phase.AtLeast(state.PhaseReview) {
		o.Display.Step("Reviewing PRs...", "(cached)")
		return nil
	}

	if err := o.reviewLoop(ctx, backend, devAgent, testAgent, devPR, testPR); err != nil {
		return err
	}

	return s.Advance(state.PhaseReview)
}

func (o *Orchestrator) reviewLoop(ctx context.Context, backend agent.Backend, devAgent *agent.DevAgent, testAgent *agent.TestAgent, devPR, testPR *gh.PR) error {
	o.Display.Info("Reviewing PRs...")

	devResult, err := review.ReviewPR(ctx, backend, devAgent.Workdir, o.Owner, o.Repo, devPR.Number, "dev")
	if err != nil {
		return fmt.Errorf("reviewing dev PR: %w", err)
	}

	if devResult.Clean {
		o.Display.Step(fmt.Sprintf("PR #%d", devPR.Number), "")
	} else {
		count := review.CountIssues(devResult.Feedback)
		o.Display.StepWarn(fmt.Sprintf("PR #%d", devPR.Number), fmt.Sprintf("%d comments posted", count))
		if err := devAgent.Fix(ctx, devResult.Feedback); err != nil {
			return fmt.Errorf("dev fix: %w", err)
		}
		if err := git.Push(ctx, devAgent.Workdir, "origin", devAgent.Branch); err != nil {
			return fmt.Errorf("pushing dev fixes: %w", err)
		}
	}

	testResult, err := review.ReviewPR(ctx, backend, testAgent.Workdir, o.Owner, o.Repo, testPR.Number, "test")
	if err != nil {
		return fmt.Errorf("reviewing test PR: %w", err)
	}

	if testResult.Clean {
		o.Display.Step(fmt.Sprintf("PR #%d", testPR.Number), "")
	} else {
		count := review.CountIssues(testResult.Feedback)
		o.Display.StepWarn(fmt.Sprintf("PR #%d", testPR.Number), fmt.Sprintf("%d comments posted", count))
		if err := testAgent.Fix(ctx, testResult.Feedback); err != nil {
			return fmt.Errorf("test fix: %w", err)
		}
		if err := git.Push(ctx, testAgent.Workdir, "origin", testAgent.Branch); err != nil {
			return fmt.Errorf("pushing test fixes: %w", err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Phase: Cross-validation + Deliver
// ---------------------------------------------------------------------------

func (o *Orchestrator) phaseValidate(ctx context.Context, s *state.RunState, devAgent *agent.DevAgent, testAgent *agent.TestAgent, issue *gh.Issue) error {
	if s.Phase.AtLeast(state.PhaseDone) {
		return nil
	}

	maxCycles := o.Config.Validate.MaxCycles
	maxRounds := o.Config.Validate.MaxFixRounds

	startCycle := s.ValCycle
	if startCycle == 0 {
		startCycle = 1
	}
	startRound := s.ValRound
	if startRound == 0 {
		startRound = 1
	}

	for cycle := startCycle; cycle <= maxCycles; cycle++ {
		roundStart := 1
		if cycle == startCycle {
			roundStart = startRound
		}
		for round := roundStart; round <= maxRounds; round++ {
			s.ValCycle = cycle
			s.ValRound = round
			_ = s.Save()

			label := fmt.Sprintf("Cross-validation (round %d)", round)

			result, err := validate.CrossValidate(ctx, o.RepoRoot, s.DevBranch, s.TestBranch, s.BaseBranch, o.Config.Test.Command)
			if err != nil {
				o.Display.StepFail(label, err.Error())
				return err
			}

			if result.Passed {
				passed := result.TotalCount
				if passed == 0 {
					passed = result.FailCount
				}
				o.Display.Step(label, fmt.Sprintf("%d/%d passed", passed, passed))

				if err := o.deliver(ctx, s, issue); err != nil {
					return err
				}
				return nil
			}

			attr := result.Attribution
			if len(attr) > 0 {
				attr = strings.ToUpper(attr[:1]) + attr[1:]
			}
			o.Display.StepFail(label, fmt.Sprintf(
				"%d failed → %s Agent fixing", result.FailCount, attr))

			if err := o.fixFromValidation(ctx, devAgent, testAgent, result); err != nil {
				return err
			}
			validate.Cleanup(ctx, o.RepoRoot)
		}
	}

	summary := fmt.Sprintf(
		"Star Fleet was unable to deliver a passing implementation after %d cycles × %d rounds.\n\nThe pipeline has been halted. Manual intervention is required.",
		maxCycles, maxRounds)
	_ = gh.PostComment(ctx, o.Owner, o.Repo, o.Number, "## ⚠️ Star Fleet — Pipeline Exhausted\n\n"+summary)
	o.Display.FailResult("Pipeline exhausted after max retries. Failure summary posted to issue.")
	return fmt.Errorf("pipeline exhausted")
}

func (o *Orchestrator) deliver(ctx context.Context, s *state.RunState, issue *gh.Issue) error {
	finalBranch := fmt.Sprintf("fleet/deliver/%d", o.Number)

	finalDir, err := git.CreateWorktree(ctx, o.RepoRoot, "deliver", finalBranch)
	if err != nil {
		return fmt.Errorf("creating delivery worktree: %w", err)
	}
	defer func() {
		_ = git.RemoveWorktree(ctx, o.RepoRoot, "deliver")
	}()

	if err := git.Merge(ctx, finalDir, s.DevBranch); err != nil {
		return fmt.Errorf("merging dev into delivery: %w", err)
	}
	if err := git.Merge(ctx, finalDir, s.TestBranch); err != nil {
		return fmt.Errorf("merging test into delivery: %w", err)
	}

	if err := git.Push(ctx, finalDir, "origin", finalBranch); err != nil {
		return fmt.Errorf("pushing delivery branch: %w", err)
	}

	body := fmt.Sprintf(
		"## Star Fleet Delivery\n\nCloses #%d\n\n"+
			"This PR was generated by Star Fleet. "+
			"Implementation and tests were written by independent agents and cross-validated.\n\n"+
			"### Included PRs\n"+
			"| Role | PR | Branch |\n"+
			"|------|-----|--------|\n"+
			"| Implementation | #%d | `%s` |\n"+
			"| Tests | #%d | `%s` |\n",
		o.Number,
		s.DevPR.Number, s.DevBranch,
		s.TestPR.Number, s.TestBranch)

	pr, err := o.findOrCreatePR(ctx, finalDir,
		fmt.Sprintf("#%d %s", o.Number, issue.Title),
		body,
		s.BaseBranch, finalBranch)
	if err != nil {
		return fmt.Errorf("creating final PR: %w", err)
	}

	s.DeliverPR = &state.PRInfo{Number: pr.Number, URL: pr.URL}
	o.Display.Result(pr.URL)

	// Close intermediate PRs — they're merged into the delivery branch
	closeMsg := fmt.Sprintf("Merged into delivery PR #%d.", pr.Number)
	_ = gh.PostComment(ctx, o.Owner, o.Repo, s.DevPR.Number, closeMsg)
	_ = gh.ClosePR(ctx, o.Owner, o.Repo, s.DevPR.Number)
	_ = gh.PostComment(ctx, o.Owner, o.Repo, s.TestPR.Number, closeMsg)
	_ = gh.ClosePR(ctx, o.Owner, o.Repo, s.TestPR.Number)

	// Issue is NOT closed here — "Closes #N" in PR body auto-closes on merge
	return s.Advance(state.PhaseDone)
}

func (o *Orchestrator) fixFromValidation(ctx context.Context, devAgent *agent.DevAgent, testAgent *agent.TestAgent, result *validate.Result) error {
	feedback := fmt.Sprintf("Cross-validation failed. Test output:\n\n```\n%s\n```\n\nFix the issues so all tests pass.", result.Output)

	switch result.Attribution {
	case "dev":
		if err := devAgent.Fix(ctx, feedback); err != nil {
			return fmt.Errorf("dev fix: %w", err)
		}
		if err := git.Push(ctx, devAgent.Workdir, "origin", devAgent.Branch); err != nil {
			return fmt.Errorf("pushing dev fixes: %w", err)
		}
	case "test":
		if err := testAgent.Fix(ctx, feedback); err != nil {
			return fmt.Errorf("test fix: %w", err)
		}
		if err := git.Push(ctx, testAgent.Workdir, "origin", testAgent.Branch); err != nil {
			return fmt.Errorf("pushing test fixes: %w", err)
		}
	}
	return nil
}

func (o *Orchestrator) cleanup(ctx context.Context) {
	_ = git.RemoveWorktree(ctx, o.RepoRoot, "dev")
	_ = git.RemoveWorktree(ctx, o.RepoRoot, "test")
	_ = git.RemoveWorktree(ctx, o.RepoRoot, "validate")
}
