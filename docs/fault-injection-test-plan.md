# Fault Injection Test Plan

> The published [failure catalog](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/)
> is the baseline contract for expected behavior. Every test below is anchored to a catalog page, the
> controller's safety-check rules (C1–C8), and the deadline/flag names the
> docs already define. Faults with no matching catalog page are **not**
> turned into test requirements here — each gets a short proposed contract
> to agree on first. Anything that would require new IRO behavior (not just
> a new test) is flagged as needing its own implementation scope, separate
> from this plan.

## 1. Background

Current testing covers controller decisions with fakes, and the test tiers
IRO should run on are defined, but there is no agreed list of real faults
IRO must handle in production, each needing an injection method,
prerequisites, and measurable pass criteria, run across two tiers (kind +
fake engine per PR, nightly GPU). The fake engine needed for the kind tier
doesn't exist yet, and GPU coverage today is manual.

The test plan treats the 15 published failure pages as the source of
truth for "what should happen," and adds only the testing mechanics
(injection method, prerequisites, pass criteria) on top.

## 2. How this plan is organized

The catalog groups its 15 scenarios into four categories: **Engine
Members**, **Pods**, **Recovery Itself**, and **IRO Itself**. Section 3
below walks every one of the 15 pages in that order, each with:

- **Prerequisites** — cluster/engine state the test needs before injecting.
- **Injection (kind tier)** — how to provoke this using the fake engine's
  `Driver`/`Policy` interface and the fake workload's `Discover`, i.e. the
  same interfaces the real plugins implement, not a separate mock API.
- **Injection (GPU tier)** — how to provoke the same scenario against a
  real engine and real Pods.
- **Pass criteria** — tied to the specific controller rule, deadline flag,
  or state-machine transition the catalog page (or the linked controller
  docs) already specifies.
- **Production signal(s) it covers** — which of the reported production
  errors this scenario is the right home for, so the mapping is explicit
  instead of inferred.

Section 4 lists the production/originally-proposed faults that don't have
a matching catalog page yet, each with a short draft contract proposal for
discussion — these stay out of the test matrix until a page exists and the
behavior is agreed. Section 5 proposes a smaller model for cheaper GPU-tier
runs. Section 6 covers execution tiers and metrics. Section 7 is open
questions.

## 3. Catalog-anchored fault matrix

Flag/deadline names below are the defaults from the controller docs:
`--ft-diagnosis-timeout` (75s), `--ft-apply-timeout` (90s),
`--ft-verify-timeout` (2m), `--ft-unseen-timeout`, `--ft-capacity-restore`
(`reset-pod` default / `reset-all` / `release`), and rule `FlapWindow` (C6).

### Engine Members

**1. [A member dies](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/member-dies)**
- *Prerequisites*: group `serving`, N admitted members; run once with
  `Capabilities.Grow=true` and once with `Grow=false` — the two endings
  ("full again without a restart" vs. "serving with fewer members") are
  different pass criteria, not one test.
- *Injection (kind)*: fake driver's `Observe` reports one `Member` as
  `Terminal: true, Serving: false`; all others unchanged.
- *Injection (GPU)*: `SIGKILL` one worker rank's engine process.
- *Pass criteria*: `Plan` returns `Change{Remove: [deadMember]}` only after
  that member is `Terminal` (C1); the reconfig names only members present
  in the observation (C7); once `Progress=Succeeded`, the member is
  recorded excluded and survivors are `Verify`d before the group is called
  `Healthy` again (never `Healthy` while an admitted member is `Unknown`);
  the Pod holding only excluded members gets exactly one `Reset` call, and
  only after the removal reconfig finished (C8).
- *Production signal(s)*: EngineCore/worker process kill; CUDA execution
  error, first occurrence; Gloo peer loss, when the killed rank is also the
  one that actually died (see #6 for the misattribution case).

**2. [Several members die at once](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/several-members-die)**
- *Prerequisites*: same as #1, ≥2 members.
- *Injection (kind)*: flip two `Member`s to `Terminal` in the same
  `Observe` call.
- *Injection (GPU)*: kill two worker ranks together.
- *Pass criteria*: a single `Change` reconfig removes both — not two
  sequential reconfigs (C7's "one reconfig at a time" means the second
  death must be folded into the same plan, not queued); one `Reset`/
  `ResetAll` call names every failed Pod at once.
- *Production signal(s)*: a correlated fault that kills multiple ranks in
  one event (e.g. a shared-node hardware fault, or a collective abort that
  takes down more than one rank).

**3. [Another member dies after a shrink](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/another-member-dies)**
- *Prerequisites*: start from a group already degraded (run #1 first, then
  continue from its end state).
- *Injection (kind/GPU)*: kill a second, different member while the group
  is `serving` below full.
- *Pass criteria*: the second reconfig plans only over currently admitted
  members; the already-excluded member is never re-checked or re-named
  (C4: "excluded members stay Terminal, an unknown one is not checked").
- *Production signal(s)*: a repeat fault shortly after a prior recovery —
  this is also where **row 16 of the original draft (repeated scale-down)**
  belongs; track any known flakiness here against this specific page
  rather than as a separate matrix entry.

**4. [A transient fault](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/transient-fault)**
- *Prerequisites*: group fully serving.
- *Injection (kind)*: flip every `Member` to `Serving: false` (paused),
  none `Terminal`, for one poll, then revert.
- *Injection (GPU)*: brief `SIGSTOP`/`SIGCONT` on all ranks, or a collective
  stall that clears on its own within one poll interval.
- *Pass criteria*: `Plan` returns a same-members `Change` (a no-op retry);
  if the fault repeats faster than it clears, C6 must block resending an
  unchanged reconfig within `FlapWindow` — assert this explicitly, it's the
  main defense against a flapping fault spamming reconfigs. Ends with the
  exact same members serving.
- *Production signal(s)*: NCCL watchdog stall (self-clearing), worker RPC
  timeout (transient), distributed network error (brief blip that clears
  inside the detection window). This is also the negative test for
  false-positive suppression.

**5. [A member hangs](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/member-hangs)**
- *Prerequisites*: group fully serving.
- *Injection (kind)*: flip `Member`s to paused (not `Terminal`); fake
  `Policy.Plan` returns `Wait`.
- *Injection (GPU)*: hang one rank's RPC/collective participation without
  exiting, and don't release it.
- *Pass criteria*: group stays `recovering` until `--ft-diagnosis-timeout`
  (75s default) elapses with `Policy` still returning `Wait`, then a full
  group reset — not a targeted remove, since nothing proved the member
  `Terminal`. Detect-to-reset should land at diagnosis timeout + ~1 poll
  interval, not earlier (would violate "never guess Terminal") and not
  materially later.
- *Production signal(s)*: worker RPC timeout (persistent), NCCL watchdog
  stall (never clears), DeepEP dispatch timeout (EP>1, stall tagged to
  dispatch — same contract, no separate EP-aware behavior is documented
  today, see §4 if that turns out to need one).

**6. [A member cannot be read](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/member-unreadable)**
- *Prerequisites*: group fully serving.
- *Injection (kind)*: flip one `Member` to `Unknown: true`; others normal.
- *Injection (GPU)*: block only that rank's control-plane port, leaving its
  process and the rest of the group alive.
- *Pass criteria*: no `Change` is ever sent naming that member while
  `Unknown` ("a `Change` that names an unknown participant is not sent");
  group resets at the diagnosis deadline if it stays unknown. Critically,
  the member must **not** be classified `Terminal` from one peer's dropped
  link alone.
- *Production signal(s)*: this is the right home for **Gloo peer loss**,
  specifically the misattribution case — a peer reporting a connection
  reset names a rank that is actually still alive, just unreachable from
  that one link. Also covers distributed network error scoped to a single
  rank.

### Pods

**7. [A Pod is lost](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/pod-lost)**
- *Prerequisites*: group serving, real workload driver (Pod-level, not just
  engine-level).
- *Injection (kind)*: fake workload's `Discover` stops listing one Pod.
- *Injection (GPU)*: delete the Pod, or take down the node under it.
- *Pass criteria*: controller injects a synthetic `Terminal: true, Detail:
  "pod lost"` member before `Plan`/`Progress`; removes it in one reconfig;
  admits the replacement Pod's members once `Ready`. Test both outcomes —
  "full again" (single-Pod replace) and "a new group" (workload recreates
  the whole group) — since the workload decides which, not IRO.
- *Production signal(s)*: node loss. NIC loss only belongs here if it's
  severe enough that kubelet actually evicts/marks the Pod lost — a flaky
  NIC that doesn't take the Pod down belongs in #6 instead (see §4 for
  proposing that split explicitly in the catalog).

**8. [The failed Pod cannot come back](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/capacity-not-restored)**
- *Prerequisites*: run as a continuation of #1 or #7's reset step.
- *Injection (kind)*: fake workload's `Reset` returns `ErrUnsupported`, or
  the replacement Pod's member never becomes `Joinable`.
- *Injection (GPU)*: same, forced via a workload config that can't satisfy
  the replacement (e.g. an unschedulable replacement Pod).
- *Pass criteria*: member marked rejected, Pod not retried; group stays
  below full until the next full reset under the default
  `--ft-capacity-restore=reset-pod`. Separately test `reset-all`: it must
  force a full reset instead and never leave the group below full — these
  are two different pass criteria, not one test with two flag values
  glossed over.
- *Production signal(s)*: no direct production signal reported yet; this
  is a capacity-restoration contract test, not a fault-detection one.

### Recovery Itself

**9. [A reconfig fails](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/reconfig-fails)**
- *Injection (kind)*: fake engine's `Progress` returns `Failed, Scope:
  Group` for an in-flight reconfig.
- *Injection (GPU)*: kill a second rank mid-reconfig such that the apply
  path reports the whole attempt broken, not just the joiners.
- *Pass criteria*: whole group resets; no partially-applied membership
  persists afterward.
- *Production signal(s)*: rendezvous timeout, when the engine itself
  reports the failure as group-scoped rather than a plain timeout (see #10
  for the timeout-without-an-explicit-Failed case).

**10. [A reconfig never finishes](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/reconfig-hangs)**
- *Injection (kind)*: fake engine's `Progress` stays `Running` past a
  shortened test `--ft-apply-timeout`.
- *Injection (GPU)*: block the rejoin/rendezvous path so `Apply` never
  completes.
- *Pass criteria*: reset triggers at `--ft-apply-timeout` (90s default) + 1
  poll, not materially earlier or later.
- *Production signal(s)*: rendezvous timeout, general case.

**11. [The group cannot generate after a recovery](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/verify-fails)**
- *Injection (kind)*: fake engine's `Verify` fails for named members even
  though `Progress=Succeeded`.
- *Injection (GPU)*: force a reconfig to apply cleanly but leave one rank
  unable to actually serve tokens.
- *Pass criteria*: retries for `--ft-verify-timeout` (2m default), then
  resets with reason `verify_failed`; group must not be marked `serving`
  while verify is still failing.
- *Production signal(s)*: a successful-looking rejoin followed by a silent
  failure that doesn't repeat as a process exit — closest match for a CUDA
  fault that corrupts state without crashing the process.

**12. [The engine cannot recover in place](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/unrecoverable)**
- *Injection (kind)*: fake `Policy.Plan` returns `Unrecoverable` directly.
- *Injection (GPU)*: the insufficient-expert-redundancy case — a
  scale-down that would leave fewer expert slots than logical experts,
  which the engine's own policy should refuse.
- *Pass criteria*: immediate full reset; no partial attempt at a reconfig
  the policy already says can't work.
- *Production signal(s)*: DeepEP dispatch timeout, specifically the
  redundancy-exhausted branch — this already has partial manual validation
  and should stay a locked-in regression.

### IRO Itself

**13. [The IRO leader dies mid-reconfig](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/leader-dies)**
- *Injection (kind/GPU)*: kill the leader (or force a journal save
  conflict) at each of three points: before `Apply`, after `Apply` but
  before acceptances are saved, after acceptances are saved but before
  `Progress=Succeeded`.
- *Pass criteria*: no acceptance saved + nothing reached the engine → new
  leader resends the same reconfig ID; acceptance saved → new leader
  watches `Progress` instead of resending. Must produce "never sent twice"
  for every crash point.
- *Production signal(s)*: operator crash / leader failover, mid-round.
  Note: the page as written covers the reconfig-in-flight case; the *idle*
  failover case (no reconfig in flight) isn't its own catalog page today —
  test it against the controller docs' general phase-continuation rule,
  but flag to catalog maintainers that it may deserve its own page or an
  explicit addition to this one.

**14. [IRO cannot see the group](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/iro-blind)**
- *Injection (kind)*: fake engine driver's `Observe` returns no admitted
  members while fake workload's `Discover` keeps succeeding normally.
- *Injection (GPU)*: block only the operator-to-engine path (e.g. a
  NetworkPolicy to the engine Pods' control port), leaving the
  operator-to-API-server path intact.
- *Pass criteria*: deadlines pause (not reset immediately) while `Discover`
  keeps working; `--ft-unseen-timeout` only counts time while `Discover`
  succeeds and `Observe` returns nothing; time during which `Discover`
  itself is failing must be excluded from that clock — assert this
  distinction explicitly, it's easy to get backwards in an implementation.
- *Production signal(s)*: distributed network error, scoped to the
  operator's own connectivity rather than inter-rank connectivity.

**15. [The journal is lost](https://llm-d-incubation.github.io/llm-d-resiliency-operator/docs/failures/journal-lost)**
- *Injection (kind/GPU)*: delete the journal's backing record while the
  group serves.
- *Pass criteria*: controller starts over, re-admits every currently-
  serving member without a reset; ends "the same group, never reset."
- *Production signal(s)*: no direct production signal reported; this is an
  IRO-internal-state test, not an engine-fault test.

## 4. Faults without a matching catalog page — proposed contracts, not yet test requirements

Per the existing-catalog-first approach, none of the following become test
requirements until a failure page exists and the behavior below (or
something the maintainers agree on instead) is accepted. Several also
imply new IRO behavior, not just a new test — those are marked explicitly
and need their own scoped design, separate from this plan.

**A. GPU Xid / hardware fault.** No existing page. Proposed contract to
discuss: a `Member` (or a side-channel health feed) needs a way to carry
"this process died because the hardware under it is unhealthy" distinctly
from a clean exit, and the catalog needs to say whether *excluding that
node from future scheduling* is IRO's responsibility or the workload's/
cluster's. **New capability** — likely touches the engine-driver
`Observation` shape and possibly the workload contract; needs its own
design, not just a doc page.

**B. Repeated CUDA execution error on the same rank/node.** No existing
page covers "retry once, then stop retrying this specific failing
rank/node." Today `Member` carries no repeat-failure count. Proposed
contract: decide whether this is observability-only (surfaced via
`Detail`/metrics for a human/alert to act on) or whether IRO itself needs a
per-member failure counter and a cordon-like state. **New capability if the
latter** — flag for separate scoping before any test is written against
it.

**C. Rendezvous timeout during replacement-Pod startup (not mid-apply).**
Distinct from #10 above: a brand-new Pod stuck in `Starting` forever has no
documented deadline — the member-dies page explicitly says Pod reset "has
no deadline; scheduling and model loading take as long as they take."
Proposed contract: decide whether that's intentional (trust the workload's
own restart budget) or whether IRO needs a bounded wait here too. Needs a
decision before testing either way, since "pass" and "fail" are currently
undefined for this case.

**D. Journal conflict as its own scenario.** The mechanics (CAS,
`ErrConflict`, reload-and-continue) are already fully specified on the
journal component page, but there's no `failures/` page walking through
"two leaders briefly overlap" in the same "what IRO sees / what IRO does /
ends with" shape as the other 15. Proposed: add that page using the
existing journal-component language verbatim — this is a documentation
gap, not a behavior gap, so it should be quick to close and then fold into
§3 as a 16th row.

**E. RBAC gap mid-recovery.** No existing page. `Discover`, journal `Save`,
and `Apply`/`Reset` calls already return errors generically; proposed
contract: confirm that a `Forbidden` response is meant to be handled
exactly like any other call error (log, retry on next poll, no special
case) rather than something new. If that's correct, this becomes a
same-shape test against existing error handling and doesn't need new
behavior — but that needs confirming before it's added to §3.

**F. "Frontend pause" (API server down with a live engine).** Doesn't map
to one page because it depends on scope: does it affect routing to *some*
members (→ #6, member-unreadable) or does it blind `Observe` entirely (→
#14, IRO-cannot-see-the-group)? Proposed: the catalog should state which,
rather than leave "frontend pause" as an undefined third category.

**G. NIC loss vs. node loss, split explicitly.** Today both would likely
be exercised via #7 (pod-lost) or #6 (unreadable) depending on severity,
but the catalog doesn't say which NIC-loss severities map where. Proposed:
document the split (e.g. "NIC loss severe enough for kubelet to evict the
Pod → #7; a flaky/degraded NIC that leaves the Pod Ready → #6") so the test
for each has an unambiguous target.

## 5. Proposed lighter GPU-tier baseline model

The only GPU validation run so far used DeepSeek-V2-Lite (TP=1/DP=4/EP=4,
4x H200). That's expensive to run nightly. Proposed candidate for a
cheaper baseline, pending its own validation pass before adoption into
§3/§6: **`PrimeIntellect/qwen3-moe-tiny`** (~670M params, 16 experts, 4
active per token), starting config: single-node, two-Pod LWS, one GPU per
Pod, TP=1/DP=2/EP=2, BF16, short context, 16 redundant experts. Before
adopting it for any row in §3, validate startup, inference, shrink, and
recovery behave the same way qualitatively as on the larger model — this
model is for exercising token-generation/recovery mechanics, not for
judging output quality or accuracy.

## 6. Execution tiers and metrics

**Kind tier** (per PR, gated on paths touching the controller/LWS/RBAC):
all of §3's `(kind)` injections, via the fake engine's `Driver`/`Policy`
and fake workload's `Discover`. Fast, deterministic, blocks merge.

**Nightly GPU tier** (self-hosted runners): all of §3's `(GPU)`
injections. Start from the validated DeepSeek-V2-Lite config; move rows to
the lighter model from §5 only after it's independently validated. Does
not block merge; failures file/update an issue tagged with the scenario
number.

**Metrics captured per scenario, both tiers**:
- time to detect (fault injected → `recovering`/diagnosis entered)
- time to recover (diagnosis entered → `Verify` passes / reset completes)
- failed-request count during the window, and whether it stayed bounded to
  requests in flight on the affected member(s) rather than fleet-wide

## 7. Open questions

1. Scenario #13's idle-failover variant and §4.D's journal-conflict page
   both look like quick documentation additions rather than new behavior —
   worth confirming that read is correct before either is written up, in
   case either hides a real behavior gap.
2. §4.A and §4.B (Xid handling, repeated-failure cordoning) are the two
   items here that clearly need new IRO capability, not just new docs —
   these should get their own design discussion rather than being decided
   as a side effect of writing a test plan.
3. Whether §5's lighter model is viable at all depends on validating it
   first; until then, §3's GPU-tier rows should be scoped to what's
   affordable on the existing DeepSeek-V2-Lite baseline, which may mean not
   every row runs nightly from day one.
