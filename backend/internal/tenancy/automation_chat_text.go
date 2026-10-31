package tenancy

import (
	"context"
	"errors"
	"time"

	"git.subcult.tv/PatrickFanella/over-laid/internal/engine"
	"git.subcult.tv/PatrickFanella/over-laid/internal/variables"
)

// OperatorChatText is private, ephemeral rehearsal output. CaptureVersion
// describes the unchanged retained source, not a new saved variable format.
// Only its original database clock is available to the variable renderer.
type OperatorChatText struct {
	CaptureVersion, SyntaxVersion int
	SelectedVersionID             string
	Run                           OperatorRun
	Text                          engine.ChatTextRehearsal
}

// RehearseOperatorChatText resolves an original actor/request and exact selected
// version. No caller capture, report, graph or execution callback is accepted.
// A capture-1 source's zero seed is a format placeholder and is never supplied
// as captured randomness; capture 2 supplies the seed chosen at admission. This read creates no admission, trace or effect.
func (s *AutomationStore) RehearseOperatorChatText(ctx context.Context, actor, request string, mode engine.Mode, expected string, syntax int) (out OperatorChatText, failure error) {
	callerCtx := ctx
	defer func() {
		if failure != nil {
			out = OperatorChatText{}
			if callerCtx != nil && callerCtx.Err() != nil {
				failure = callerCtx.Err()
				return
			}
			for _, fixed := range []error{engine.ErrChatText, engine.ErrChatTextUnsupported, engine.ErrChatTextUnavailable, engine.ErrChatTextLimit} {
				if errors.Is(failure, fixed) {
					failure = fixed
					return
				}
			}
			failure = automationRunError(callerCtx, failure)
		}
	}()
	if ctx == nil || s == nil || s.db == nil || !automationID(actor) || !automationID(request) || !automationID(expected) || syntax != variables.SyntaxVersion || (mode != engine.ModeReproduce && mode != engine.ModeCurrentConfig) {
		return out, ErrAutomationInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.validateRuns(ctx); err != nil {
		return out, err
	}
	tx, err := s.beginOperator(ctx, actor, false)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := lookupOperatorRun(ctx, tx, operatorOccurrence(s.db.TenantID(), actor, request), actor)
	if err != nil {
		return out, err
	}
	graph, input, err := s.operatorRehearsalInput(ctx, tx, run, mode)
	if err != nil {
		return out, err
	}
	selected := graph.Root().Document().VersionID
	if selected != expected {
		return out, ErrAutomationConflict
	}
	snapshot, err := operatorSnapshot(input)
	if err != nil {
		return out, ErrAutomationInvalid
	}
	text, err := engine.RehearseChatText(ctx, graph, engine.ChatTextInput{Flow: input, Variables: snapshot, SyntaxVersion: syntax})
	if err != nil {
		return out, err
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	run.Unchanged = true
	return OperatorChatText{CaptureVersion: run.CaptureVersion(), SyntaxVersion: syntax, SelectedVersionID: selected, Run: run, Text: text}, nil
}

// operatorSnapshot supplies the original clock and, for capture 2, the seed
// chosen at admission. A zero seed is never passed as captured randomness.
func operatorSnapshot(input engine.RehearsalInput) (variables.Snapshot, error) {
	in := variables.SnapshotInput{Clock: &input.Clock}
	if input.Seed > 0 {
		seed := input.Seed
		in.Seed = &seed
	}
	return variables.NewSnapshot(in)
}
