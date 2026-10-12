# Fault Injection Test Plan

> Proposal for issue #31 (fault matrix and pass criteria), building on the
> test-tier structure from issue #20 and the manual GPU validation results
> from PR #11.

## 1. Background

Current testing covers controller decisions with fakes, and the test tiers
IRO should run on are defined, but there is no agreed list of real faults
IRO must handle in production — each needing an injection method, expected
response, and pass/fail criteria, run across two tiers (kind + fake engine
per PR, nightly GPU). The fake engine needed for the kind tier doesn't
exist yet, and GPU coverage today is manual.

A manual GPU validation run (DeepSeek-V2-Lite, TP=1/DP=4/EP=4, 4x H200)
already exercised several of these faults and is the best source of ground
truth we have for expected behavior and known gaps. This plan folds the
following production error signals into the matrix instead of treating
them as a separate list:

- DeepEP dispatch timeout
- Worker RPC timeout
- NCCL watchdog stall
- CUDA execution error
- Gloo peer loss
- Distributed network error
- Rendezvous timeout

None of these are new *categories* — they're sharper versions of "hung
collective," "worker kill," and "GPU error" from #31's original list, each
with a distinct detection signature IRO needs to tell apart. The matrix
below replaces #31's flat list with one organized by what IRO actually has
to distinguish at diagnosis time: **is the process alive or dead**, and
**did the fault originate in the engine, the network/collective layer, or
IRO's own recovery path**.

## 2. Fault matrix

Legend — **Action**: `none` (self-heal, no-op) / `retry` / `scale_down` /
`reset` (LWS group recreation) / `escalate` (stop retrying, alert).
**Tier**: `K` = kind + fake engine (per PR), `G` = nightly GPU (per #20).

| # | Fault | Injection — kind/fake engine | Injection — GPU tier | Detection signal IRO must key on | Expected action | Pass / fail criteria | Tier |
|---|---|---|---|---|---|---|---|
| 1 | EngineCore / worker process kill | Fault-injection API on fake engine: `SIGKILL` self | `SIGKILL` a worker rank's engine process | Process exit, non-zero/signal code | `retry` (in place) if within budget | Detect < 1 health-probe interval; recover < apply deadline; failed requests ≤ in-flight at kill time | K, G |
| 2 | Worker RPC timeout (transient) | Fake engine's RPC handler sleeps N s, then resumes | `SIGSTOP` worker briefly, then `SIGCONT` | RPC call exceeds timeout, process otherwise alive | `none` — diagnosis clears once probes succeed again | Diagnosis clears without replacing any process (mirrors #11's transient-frontend-pause result) | K, G |
| 3 | Worker RPC timeout (persistent) | Fake engine's RPC handler never resumes | `SIGSTOP` worker, never `SIGCONT` | Diagnosis deadline exceeded, no removal evidence | `reset` | Reset reason correctly recorded as the *original* timeout (regression test for the discovery-overwrite bug fixed in #11) | K, G |
| 4 | NCCL watchdog stall (hung collective, no death) | Fake engine reports "in collective" + stops advancing a progress counter, process stays up | Debug hook delays one rank's matching collective call so NCCL watchdog logs a timeout; `TORCH_NCCL_ASYNC_ERROR_HANDLING` kept at the project default | No-progress on heartbeat/step counter past deadline while process alive | `retry` the stalled round if within budget, else `reset` | Detect within no-progress deadline + margin; failed requests bounded to the in-flight round, not the whole fleet | K, G |
| 5 | DeepEP dispatch timeout (EP-specific stall) | Fake engine stall tagged `ep_dispatch` (only when EP > 1) | Drop/throttle RDMA traffic to one EP rank, or trip DeepEP's own dispatch timeout path | Same as #4 but tagged EP, feeding the EPLB redundancy check before recovery | `scale_down` if EPLB redundancy allows it, else `reset` | If redundant: in-place recovery, bounded failed requests. If not: clean rejection + reset — no hang (extends #11's "insufficient expert redundancy" case to the fault that actually causes it in prod) | K, G |
| 6 | Gloo peer loss | Fake engine's control channel closes abruptly (hard disconnect, not graceful shutdown) | `SIGKILL` one rank specifically while it holds a Gloo store connection (e.g. mid-rendezvous) | Peers report a connection-reset error that *names a different rank* than the one that actually died | Same as #1, but diagnosis must attribute the fault to the orchestrator-observed dead rank, not the reporting peer | No misattribution: IRO recovers the rank Kubernetes says is dead, not the one that merely logged the error | K, G |
| 7 | Distributed network error (transient network blip) | `tc netem` / toxiproxy between fake engine and operator for a bounded window | Same `tc netem` applied in the pod netns for a bounded window | Transient RPC/connection errors that clear inside the detection grace window | `none` | Zero recovery actions taken; this is a negative test for false-positive suppression — currently untested | K, G |
| 8 | Rendezvous timeout during recovery | Fake engine's rejoin handshake hangs past the configured rendezvous timeout, only during an IRO-triggered recovery attempt | Block the rendezvous/TCPStore port for the new LWS pods during a `reset` | IRO's own remediation fails to converge (not just the original fault persisting) | Bounded retries, then `escalate` via reset-budget-exhausted | No infinite reset loop; terminal/alertable state reached; journal shows the attempt sequence | K, G |
| 9 | CUDA execution error (process fault, no Xid) | Fake engine exits non-zero with a CUDA-error-shaped stderr signature, no Xid in the fake health feed | Trigger a reproducible device-side fault (debug kernel hook) causing an illegal-memory-access-style crash | Non-zero exit with CUDA-error signature, absent Xid/DCGM event | `retry` on first occurrence; `escalate`/cordon on repeat for the same rank+node within a window | First occurrence recovers within SLA; repeat occurrence does not retry forever | K, G |
| 10 | GPU Xid / hardware fault | Synthetic Xid record via a mock DCGM/health feed | Synthetic Xid injection via DCGM test mode if available, else non-primary NIC flap as a stand-in | Xid/DCGM event present | Mark node/rank unhealthy; exclude from retry; `scale_down`/`reset` routes around it | Bad node excluded from scheduling until health clears; distinguishes from #9 (no retry-in-place on same hardware) | G (K only covers the consumption path with a synthetic event) |
| 11 | NIC loss / node loss | N/A (no real hardware in kind) | Link-down on a worker node's NIC, or terminate the node | Node NotReady / unreachable | `reset` (and in multi-node LWS, exclude the lost node) | Recovers onto healthy nodes; failed requests bounded to requests in flight on the lost node | G |
| 12 | API server down with a live engine (frontend pause, transient) | Fake frontend stops responding to health checks for N s, then resumes | Pause/suspend the API-server container briefly | Verification clears despite diagnosis entering | `none` — resumes without replacing processes | Matches #11's transient-frontend-pause result exactly; keep as a locked-in regression | K, G |
| 13 | API server down with a live engine (frontend pause, persistent) | Fake frontend never resumes | Pause/kill the API-server container, don't restart | Diagnosis deadline exceeded, no removal evidence | `reset` | Matches #11's persistent-frontend-pause result; regression-lock the reset-reason bug fix | K, G |
| 14 | Operator crash / leader failover, idle | Kill the leader operator pod between rounds | Same | New leader elected | New leader resumes normal operation | New leader doesn't replay completed recovery (matches #11) | K, G |
| 15 | Operator crash / leader failover, **mid-round** (gap in #11) | Kill the leader operator pod while a `retry`/`scale_down`/`reset` is in flight, for each action type | Same | New leader must read journal state mid-action | New leader resumes or safely re-derives the in-flight action, exactly once | No duplicate recovery action, no abandoned action; this is explicitly *not* covered by #11 (which only tested failover after completion) and should be treated as new, not a regression check | K, G |
| 16 | Repeated scale-down on the same group | Second rank-kill injected shortly after a successful scale-down | Same, on real hardware | Apply deadline exceeded a second time | Known-flaky today per #11 (hung to the 90 s deadline) | Track as a quarantined/non-blocking case until the underlying bug is fixed; do not let it block CI, but do not silently drop it either | G (K once fake engine can reproduce the sequencing) |
| 17 | Journal conflict | Two fake leaders write conflicting journal entries (test harness forces a race) | N/A (control-plane only, no GPU needed) | Journal CAS/version conflict | Loser backs off, winner's state wins | No corrupted/merged journal state, no duplicate action execution | K |
| 18 | RBAC gap (operator missing a permission it needs mid-recovery) | Deploy IRO with the shipped RBAC and intentionally narrow a verb used by one action type | N/A | API server `Forbidden` during a recovery step | Recovery step fails loudly, reset-budget/escalation path engages rather than silent stall | No silent hang on `Forbidden`; failure surfaces in logs/metrics within one reconcile loop | K |

## 3. What the fake engine needs (for #20)

Every kind-tier row above needs the same small set of primitives from the
fake FT engine, so it's worth building this as a generic fault-injection
control surface rather than one-off hacks per test:

- `kill(rank)` — hard process exit (covers #1, #6).
- `hang(rank, duration, tag?)` — stop responding to RPC/health probes
  without exiting; `tag` distinguishes `rpc`, `collective`, `ep_dispatch`
  for #2–#5.
- `exit_with_signature(rank, code, stderr_pattern)` — covers #9's
  CUDA-error-shaped crash distinct from a plain kill.
- `block_rendezvous(duration)` — only during a recovery attempt, for #8.
- Network-level faults (#7, #11) are better done at the test-harness level
  (`tc netem`, NetworkPolicy) than inside the fake engine itself, since they
  need to also work against the real engine on the GPU tier with the same
  harness code.

Building the fake engine around these primitives means the GPU tier
(section 4) can reuse the same test harness and assertions — only the
injection mechanism changes, not what's being checked.

## 4. Two-tier execution plan

**Kind tier (per PR, gated on paths touching the controller/LWS/RBAC):**
all rows marked `K` above, using the fake engine's fault-injection API.
Fast, deterministic, blocks merge.

**Nightly GPU tier (per #20, llm-d self-hosted runners):** all rows marked
`G`, starting from the exact config #11 validated manually
(DeepSeek-V2-Lite, TP=1/DP=4/EP=4, 4x H200) so results are comparable to the
known-good baseline. Does not block merge; failures file/update an issue
tagged with the fault ID.

**Metrics captured per fault, both tiers** (per #31's ask):
- time to detect (fault injected → diagnosis state entered)
- time to recover (diagnosis entered → verification cleared / reset
  complete)
- failed request count during the window, and whether it stayed bounded to
  in-flight requests or affected steady-state traffic

**Known non-blocking caveats carried over from #11** (explicitly *not*
pass/fail gates, to avoid re-litigating settled findings):
- Output determinism can change after expert redistribution; #11 already
  noted this does not by itself prove an accuracy regression, so it's
  tracked as an observability check, not a correctness gate.
- Full leader-pod dual-engine loss falls back to group recreation instead
  of in-place recovery; row #15 above now exists to track whether mid-round
  failover makes this better, but until then this is expected, not a bug.

## 5. Open questions for #31/#20

1. Thresholds above (apply deadline, no-progress deadline, retry/reset
   budgets) reuse the ~90 s figure observed in #11's manual run as a
   placeholder — these need an owner to confirm as configured defaults
   rather than incidental test-run values.
2. Row #16 (repeated scale-down) and row #15 (mid-round leader failover)
   are the two cases with no prior passing result at all; worth confirming
   whether either blocks #20's nightly tier from going green on day one, or
   starts quarantined.
3. DeepEP-specific injection (#5) needs confirmation that the nightly
   runners' interconnect actually supports forcing a dispatch timeout
   deterministically — if not, it may need to stay a kind-tier-only
   (tagged, simulated) case until hardware access is confirmed.
