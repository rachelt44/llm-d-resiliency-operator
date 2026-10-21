// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // The flow drives the driver through its test transport.
package vllm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/telemetry"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

// cluster simulates vLLM fault tolerance across ranks: instructions only while unhealthy, one request ID
// that every alive rank must receive, and a store that moves when the lowest alive rank is removed.
type cluster struct {
	mu       sync.Mutex
	refault  bool
	ranks    []*fakeRank
	dead     map[int]bool
	received map[string]map[int]map[string]any
	applies  []string
}

func newCluster(test *fleet) *cluster {
	sim := &cluster{ranks: test.ranks, dead: map[int]bool{}, received: map[string]map[int]map[string]any{}}
	for _, fake := range test.ranks {
		fake.onApply = sim.apply
	}
	return sim
}

func (sim *cluster) apply(fake *fakeRank, payload map[string]any) {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	id := fmt.Sprint(payload["request_id"])
	sim.applies = append(sim.applies, fmt.Sprintf("%s:%v:dp%d", id, payload["instruction"], fake.rank))
	current := fake.get()
	if status := current["status"]; status != "unhealthy" {
		fake.put(map[string]any{
			"id": fake.rank, "status": status, "ft_state": "failed", "last_ft_request_id": id,
			"ft_error": fmt.Sprintf("status is %v", status),
		})
		return
	}
	fake.put(map[string]any{
		"id": fake.rank, "status": "unhealthy", "ft_state": "recovering", "last_ft_request_id": id,
	})
	if sim.received[id] == nil {
		sim.received[id] = map[int]map[string]any{}
	}
	sim.received[id][fake.rank] = payload
	removed := removedRanks(payload)
	var alive []int
	for rank := range sim.ranks {
		if !sim.dead[rank] && !removed[rank] {
			alive = append(alive, rank)
		}
	}
	for _, rank := range alive {
		if sim.received[id][rank] == nil {
			return
		}
	}
	for rank := range removed {
		sim.dead[rank] = true
	}
	for _, rank := range alive {
		sim.ranks[rank].put(map[string]any{"id": rank, "status": "healthy"})
		if sim.refault {
			sim.ranks[rank].put(map[string]any{"id": rank, "status": "unhealthy", "fault_info": "RuntimeError"})
		}
	}
}

func removedRanks(payload map[string]any) map[int]bool {
	removed := map[int]bool{}
	found, ok := payload["params"].(map[string]any)
	if !ok {
		return removed
	}
	list, ok := found["removed_dp_ranks"].([]any)
	if !ok {
		return removed
	}
	for _, rank := range list {
		if number, ok := rank.(float64); ok {
			removed[int(number)] = true
		}
	}
	return removed
}

type memoryJournal struct {
	mu      sync.Mutex
	data    []byte
	version int
}

//nolint:gocritic // The contract returns the record, its version and an error.
func (store *memoryJournal) Load(context.Context) ([]byte, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.version == 0 {
		return nil, "", nil
	}
	return slices.Clone(store.data), strconv.Itoa(store.version), nil
}

func (store *memoryJournal) Save(_ context.Context, data []byte, version string) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if (store.version == 0 && version != "") || (store.version > 0 && version != strconv.Itoa(store.version)) {
		return "", journal.ErrConflict
	}
	store.version++
	store.data = slices.Clone(data)
	return strconv.Itoa(store.version), nil
}

func (store *memoryJournal) record(t *testing.T) enginefault.Record {
	t.Helper()
	var record enginefault.Record
	data, _, err := store.Load(t.Context())
	if err == nil {
		err = json.Unmarshal(data, &record)
	}
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type lwsToday struct {
	pods      []workload.Pod
	resetAll  int
	resetCall int
}

func (wl *lwsToday) Discover(context.Context) (workload.Group, error) {
	return workload.Group{Pods: slices.Clone(wl.pods)}, nil
}

func (wl *lwsToday) Reset(context.Context, []workload.PodRef) error {
	wl.resetCall++
	return workload.ErrUnsupported
}

func (wl *lwsToday) ResetAll(context.Context, []workload.PodRef) error {
	wl.resetAll++
	return nil
}

func (*lwsToday) Release(context.Context, []workload.PodRef) error { return workload.ErrUnsupported }

type flow struct {
	t     *testing.T
	fleet *fleet
	sim   *cluster
	wl    *lwsToday
	store *memoryJournal
	fsm   enginefault.Controller
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	return newFlowWith(t, time.Second)
}

func newFlowWith(t *testing.T, applyTimeout time.Duration) *flow {
	t.Helper()
	test := newFleet(t, 4, 1)
	run := &flow{t: t, fleet: test, sim: newCluster(test), wl: &lwsToday{pods: test.pods}, store: &memoryJournal{}}
	config := enginefault.Defaults()
	config.PollInterval, config.DiagnosisTimeout, config.ApplyTimeout = time.Millisecond, time.Second, applyTimeout
	fsm, err := enginefault.New(t.Context(), run.wl, test.eng, run.store, telemetry.New(logr.Discard(), time.Minute), config)
	if err != nil {
		t.Fatal(err)
	}
	run.fsm = fsm
	run.until("serving with four ranks", func(record enginefault.Record) bool {
		return record.Phase == enginefault.Serving && len(record.Members) == 4
	})
	return run
}

func (run *flow) until(what string, done func(enginefault.Record) bool) enginefault.Record {
	run.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := run.fsm.Step(run.t.Context()); err != nil {
			run.t.Logf("step: %v", err)
		}
		if record := run.store.record(run.t); done(record) {
			return record
		}
		time.Sleep(2 * time.Millisecond)
	}
	run.t.Fatalf("never %s: %+v, applies %v", what, run.store.record(run.t), run.sim.applies)
	return enginefault.Record{}
}

func (run *flow) fault(dead ...int) {
	for rank, fake := range run.fleet.ranks {
		if slices.Contains(dead, rank) {
			fake.set("dead", nil)
		} else {
			fake.unhealthy()
		}
	}
}

func served(record *enginefault.Record) bool {
	return record.Phase == enginefault.Serving && record.Reconfig == nil && len(record.Verifying) == 0 && record.Incident == nil
}

func kept(id string) func(enginefault.Record) bool {
	return func(record enginefault.Record) bool {
		return served(&record) && record.Members[engine.MemberID(id)] == enginefault.Rejected
	}
}

func TestFlowDeadRankIsRemoved(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault(1)
	record := run.until("shrunk and kept below full", kept("dp1@id1"))
	if len(run.sim.applies) != 3 || run.wl.resetAll != 0 {
		t.Fatalf("members %v, applies %v, group resets %d", record.Members, run.sim.applies, run.wl.resetAll)
	}
	for _, apply := range run.sim.applies {
		if !strings.Contains(apply, ":scale_down:") {
			t.Fatalf("applies %v", run.sim.applies)
		}
	}
	if payload := run.fleet.ranks[0].applied[0]; params(t, payload)["dp_master_ip"] != nil {
		t.Fatalf("store moved without reason: %v", payload)
	}
}

// TestFlowMemberDiesMatchesCatalog checks the pass criteria from docs/failures/member-dies.md: one
// reconfig removes the dead rank, survivors are verified before the incident closes, and since vLLM
// cannot grow, IRO never resets the Pod and the group never resets.
func TestFlowMemberDiesMatchesCatalog(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault(3)
	record := run.until("shrunk and kept below full", kept("dp3@id3"))

	if len(run.sim.applies) != 3 {
		t.Fatalf("one reconfig should reach every survivor once, applies %v", run.sim.applies)
	}
	id, _, _ := strings.Cut(run.sim.applies[0], ":")
	for _, apply := range run.sim.applies {
		if !strings.HasPrefix(apply, id+":scale_down:") {
			t.Fatalf("applies %v are not all the same reconfig", run.sim.applies)
		}
	}
	if record.Members[engine.MemberID("dp3@id3")] != enginefault.Rejected {
		t.Fatalf("dp3 should be rejected, members %v", record.Members)
	}
	for _, survivor := range []int{0, 1, 2} {
		if run.fleet.ranks[survivor].prompts != 1 {
			t.Fatalf("survivor dp%d should be verified exactly once, prompts %d", survivor, run.fleet.ranks[survivor].prompts)
		}
	}
	if run.fleet.ranks[3].prompts != 0 {
		t.Fatalf("removed dp3 should never be verified, prompts %d", run.fleet.ranks[3].prompts)
	}
	if run.wl.resetCall != 0 {
		t.Fatalf("vLLM cannot grow: IRO must not try to reset the Pod, reset calls %d", run.wl.resetCall)
	}
	if run.wl.resetAll != 0 {
		t.Fatalf("a single dead rank must not reset the whole group, resets %d", run.wl.resetAll)
	}
}

func TestFlowStoreHostIsRemoved(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault(0)
	run.until("shrunk", func(record enginefault.Record) bool { return served(&record) })
	found := params(t, run.fleet.ranks[2].applied[0])
	if found["dp_master_ip"] != "10.0.0.2" || found["dp_store_port"] != 29601.0 {
		t.Fatalf("params %v", found)
	}
}

func TestFlowTransientFaultIsRetried(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault()
	record := run.until("recovered", func(record enginefault.Record) bool { return served(&record) })
	if len(run.sim.applies) != 4 || !strings.Contains(run.sim.applies[0], ":retry:") || len(record.Members) != 4 {
		t.Fatalf("applies %v, members %v", run.sim.applies, record.Members)
	}
}

func TestFlowSecondFaultRemovesAnotherRank(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault(3)
	run.until("shrunk once", func(record enginefault.Record) bool { return served(&record) })
	for rank, fake := range run.fleet.ranks[:3] {
		if rank == 1 {
			fake.set("dead", nil)
		} else {
			fake.unhealthy()
		}
	}
	record := run.until("shrunk twice", kept("dp1@id1"))
	if run.wl.resetAll != 0 || len(run.sim.applies) != 5 {
		t.Fatalf("members %v, applies %v, group resets %d", record.Members, run.sim.applies, run.wl.resetAll)
	}
}

func TestFlowWedgedRankResetsTheGroup(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault(1)
	run.fleet.ranks[3].set("healthy", nil)
	run.until("reset", func(record enginefault.Record) bool { return record.Phase == enginefault.Starting })
	if run.wl.resetAll == 0 || len(run.sim.applies) != 0 {
		t.Fatalf("group resets %d, applies %v", run.wl.resetAll, run.sim.applies)
	}
}

func TestFlowEngineProcessGone(t *testing.T) {
	t.Parallel()
	run := newFlow(t)
	run.fault()
	run.fleet.ranks[2].set("healthy", nil)
	run.fleet.ranks[2].health = http.StatusServiceUnavailable
	run.until("shrunk and kept below full", kept("dp2@id2"))
}

func TestFlowFaultAfterTheReconfigResetsAtOnce(t *testing.T) {
	t.Parallel()
	run := newFlowWith(t, 30*time.Second)
	run.sim.refault = true
	run.fault(3)
	started := time.Now()
	run.until("reset", func(record enginefault.Record) bool { return record.Phase == enginefault.Starting })
	if run.wl.resetAll == 0 || time.Since(started) > 5*time.Second {
		t.Fatalf("group resets %d after %v", run.wl.resetAll, time.Since(started))
	}
}
