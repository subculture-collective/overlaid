package tenancy

import (
	"encoding/json"
	"fmt"
	"testing"

	"git.subcult.tv/PatrickFanella/over-laid/internal/engine"
)

func TestOperatorSeededRandomCaptureAndLegacyOccurrences(t *testing.T) {
	s, a, _, owner, _, f := tenantPair(t)
	store, _ := automationStore(t, s, a.ID)
	ctx := t.Context()
	d := chatPlanVersion(t, a.ID, 875, 8750, "x", 1).Document()
	d.Nodes = []engine.Node{
		{ID: "root", Kind: "random", Children: []string{"heads", "tails"}, Weights: []int{1, 1}},
		{ID: "heads", Kind: "action", Capability: "chat.send", Arguments: []engine.Argument{{Name: "text", Value: "heads"}}},
		{ID: "tails", Kind: "action", Capability: "chat.send", Arguments: []engine.Argument{{Name: "text", Value: "tails"}}},
	}
	v, err := engine.Validate(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Publish(ctx, owner, v, ""); err != nil {
		t.Fatal(err)
	}
	request := operatorRequest(875)
	run, err := store.AdmitOperator(ctx, owner, request, d.AutomationID, d.VersionID)
	if err != nil || run.Seed == 0 || run.Seed > MaxOperatorSeed || run.CaptureVersion() != 3 || len(run.Recent) != 0 {
		t.Fatal("admitted seed", run, err)
	}
	// A duplicate admission returns the original seed, never a fresh draw.
	if again, err := store.AdmitOperator(ctx, owner, request, d.AutomationID, d.VersionID); err != nil || again.Seed != run.Seed {
		t.Fatal("duplicate seed", again, err)
	}
	first, err := store.RehearseOperator(ctx, owner, request, engine.ModeReproduce)
	if err != nil || first.CaptureVersion != 3 || first.Flow.Seed != run.Seed || first.Flow.Status != "proposed" || len(first.Flow.Proposals) != 1 {
		t.Fatal("seeded rehearsal", first, err)
	}
	second, err := store.RehearseOperator(ctx, owner, request, engine.ModeReproduce)
	if err != nil || second.Flow.Proposals[0].NodeID != first.Flow.Proposals[0].NodeID || second.Flow.CaptureDigest != first.Flow.CaptureDigest {
		t.Fatal("seeded rehearsal not reproducible", second, err)
	}
	saved, err := store.SaveOperatorTrace(ctx, owner, request, engine.ModeReproduce, d.VersionID)
	if err != nil || saved.Trace.CaptureVersion != 3 || saved.Trace.Seed != run.Seed {
		t.Fatal("seeded trace", saved, err)
	}
	if loaded, err := store.LoadOperatorTrace(ctx, owner, request, saved.Trace.ID); err != nil || loaded.Digest != saved.Digest {
		t.Fatal("seeded trace load", loaded, err)
	}
	// A trace whose seed disagrees with its run is refused, never repaired.
	if _, err = f.Admin.Exec(`UPDATE engine_operator_occurrences SET seed=seed%9007199254740991+1 WHERE tenant_id=$1 AND id=$2`, a.ID, run.OccurrenceID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.LoadOperatorTrace(ctx, owner, request, saved.Trace.ID); err == nil {
		t.Fatal("trace accepted a different seed")
	}
	// Memory needs a seed, and occurrences admitted before memory existed keep
	// capture 2 with their seed.
	if _, err = f.Admin.Exec(`UPDATE engine_operator_occurrences SET seed=0 WHERE tenant_id=$1 AND id=$2`, a.ID, run.OccurrenceID); err == nil {
		t.Fatal("memory without a seed accepted")
	}
	if _, err = f.Admin.Exec(`UPDATE engine_operator_occurrences SET recent='null' WHERE tenant_id=$1 AND id=$2`, a.ID, run.OccurrenceID); err != nil {
		t.Fatal(err)
	}
	if prior, err := store.RehearseOperator(ctx, owner, request, engine.ModeReproduce); err != nil || prior.CaptureVersion != 2 || prior.Flow.Status != "proposed" {
		t.Fatal("capture 2", prior, err)
	}
	// Occurrences admitted before seeds existed keep capture 1: random stays
	// unsupported and the zero seed is never treated as captured randomness.
	if _, err = f.Admin.Exec(`UPDATE engine_operator_occurrences SET seed=0 WHERE tenant_id=$1 AND id=$2`, a.ID, run.OccurrenceID); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.RehearseOperator(ctx, owner, request, engine.ModeReproduce)
	if err != nil || legacy.CaptureVersion != 1 || legacy.Flow.Seed != 0 || legacy.Flow.Status != "unsupported" || len(legacy.Flow.Proposals) != 0 {
		t.Fatal("legacy capture", legacy, err)
	}
	if _, err = f.Admin.Exec(`UPDATE engine_operator_occurrences SET seed=-1 WHERE tenant_id=$1 AND id=$2`, a.ID, run.OccurrenceID); err == nil {
		t.Fatal("negative seed accepted")
	}
}

func TestOperatorNoRepeatMemoryAcrossOccurrences(t *testing.T) {
	s, a, _, owner, _, f := tenantPair(t)
	store, _ := automationStore(t, s, a.ID)
	ctx := t.Context()
	send := func(id string) engine.Node {
		return engine.Node{ID: id, Kind: "action", Capability: "chat.send", Arguments: []engine.Argument{{Name: "text", Value: id}}}
	}
	d := chatPlanVersion(t, a.ID, 876, 8760, "x", 1).Document()
	// "c" carries nearly all of the weight, so only no-repeat keeps it from
	// winning twice in a row.
	d.Nodes = []engine.Node{
		{ID: "root", Kind: "random", NoRepeat: true, Children: []string{"a", "b", "c"}, Weights: []int{1, 1, 999_998}},
		send("a"), send("b"), send("c"),
	}
	v, err := engine.Validate(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Publish(ctx, owner, v, ""); err != nil {
		t.Fatal(err)
	}
	key := engine.RandomKey(d.AutomationID, "root")
	memory := func() map[string]string {
		t.Helper()
		var raw []byte
		if err := f.Admin.QueryRow(`SELECT recent FROM engine_run_budgets WHERE tenant_id=$1 AND automation_id=$2`, a.ID, d.AutomationID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	picks := []string{}
	for i := range 6 {
		request := operatorRequest(880 + i)
		run, err := store.AdmitOperator(ctx, owner, request, d.AutomationID, d.VersionID)
		if err != nil || run.CaptureVersion() != 3 {
			t.Fatal("admit", i, run, err)
		}
		// The occurrence captured the memory as it was before its own pick.
		if i == 0 && len(run.Recent) != 0 || i > 0 && run.Recent[key] != picks[i-1] {
			t.Fatal("captured memory", i, run.Recent, picks)
		}
		r, err := store.RehearseOperator(ctx, owner, request, engine.ModeReproduce)
		if err != nil || r.CaptureVersion != 3 || r.Flow.Status != "proposed" || len(r.Flow.Proposals) != 1 || r.Flow.Steps[1].Reason != "no_repeat_choice" {
			t.Fatal("rehearse", i, r, err)
		}
		pick := r.Flow.Proposals[0].NodeID
		// The kept memory matches the pick every rehearsal of this occurrence makes.
		if memory()[key] != pick {
			t.Fatal("kept memory", i, memory(), pick)
		}
		if i > 0 && pick == picks[i-1] {
			t.Fatal("repeated pick", i, picks, pick)
		}
		picks = append(picks, pick)
		// Later admissions never change what an earlier occurrence replays.
		if i > 0 {
			prior, err := store.RehearseOperator(ctx, owner, operatorRequest(880+i-1), engine.ModeReproduce)
			if err != nil || prior.Flow.Proposals[0].NodeID != picks[i-1] {
				t.Fatal("earlier occurrence changed", i, prior, err)
			}
		}
	}
	if saved, err := store.SaveOperatorTrace(ctx, owner, operatorRequest(885), engine.ModeReproduce, d.VersionID); err != nil || saved.Trace.CaptureVersion != 3 {
		t.Fatal("trace", saved, err)
	}
	// A new version without the node drops its memory on the next admission.
	previous := d.VersionID
	d.VersionID = fmt.Sprintf("%08x-2222-4222-8222-222222222222", 8761)
	d.Nodes = []engine.Node{send("root")}
	v, err = engine.Validate(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Publish(ctx, owner, v, previous); err != nil {
		t.Fatal(err)
	}
	if run, err := store.AdmitOperator(ctx, owner, operatorRequest(890), d.AutomationID, d.VersionID); err != nil || len(run.Recent) != 0 || len(memory()) != 0 {
		t.Fatal("dropped memory", run, memory(), err)
	}
	// Malformed stored memory is refused, never repaired.
	if _, err = f.Admin.Exec(`UPDATE engine_run_budgets SET recent='{"bad key":"a"}' WHERE tenant_id=$1 AND automation_id=$2`, a.ID, d.AutomationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdmitOperator(ctx, owner, operatorRequest(891), d.AutomationID, d.VersionID); err == nil {
		t.Fatal("malformed memory accepted")
	}
	if _, err = f.Admin.Exec(`UPDATE engine_run_budgets SET recent='[]' WHERE tenant_id=$1 AND automation_id=$2`, a.ID, d.AutomationID); err == nil {
		t.Fatal("non-object memory accepted")
	}
}
