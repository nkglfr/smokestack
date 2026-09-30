# Federated double-check — design note

**Status: implemented.** This note was written as a proposal, to be argued with before any code existed, because the feature asks one operator's machine to measure something on another operator's behalf — a different kind of request from everything else the federation does, and getting the consent model wrong would turn a monitoring tool into an open measurement relay.

It is kept as written rather than rewritten into documentation, because the reasoning is the useful part and a specification that hides its own trade-offs is worse than one that shows them. Two questions were settled by the operator and are marked as such below; the operational description lives in [DEPLOY.md § 13](../DEPLOY.md#13-federation-what-protects-what).

Read alongside [DESIGN.md §10 Federation](../DESIGN.md#10-federation), whose pairing, signing and nonce machinery this builds on and does not replace.

## 1. What it is for

An instance sees one of its targets fail. It cannot tell, from one vantage point, whether the target is down or whether its own transit is. A traceroute helps and often does not settle it: a path that breaks four hops away is consistent with both readings.

A paired peer sitting in a different AS can settle it in one pass. If the peer reaches the target and we do not, the fault is between us and it. If the peer does not reach it either, the target is down. That is the whole value, and it is worth something precisely at the moment an operator is least able to reason calmly.

The existing NOC alerting already does something adjacent: it warns a peer's NOC when several independent observers agree. The difference is direction and subject. NOC alerting is about **the peer's** network, corroborated by instances that already measure it as their own anchor. A double-check is about **our** target, measured by a peer that has no reason to watch it, on request, temporarily.

## 2. The problem to solve first

Left unguarded, this is a measurement relay. Instance A asks peer B to connect to an address of A's choosing; B does it and reports what happened. That is, in the general case, a way to make somebody else's machine probe a third party — and to launder the origin of that probing behind B's AS number.

So the design question is not "how do we ask a peer to measure" — that part is easy and the signing already exists. It is **what makes a request legitimate, and who gets to decide**.

Three answers combine, and none of them is sufficient alone.

### 2.1 Consent, granted in advance, per peer

The request is refused unless B's operator has previously granted A the right to ask. As you put it: an authorisation between peers to use each other automatically for double-checking. Once granted, individual requests need no human — the point is to answer during an incident, at three in the morning, not to wake a second operator.

Consequences of "in advance, per peer":

- It is **not** a federation-wide switch. Pairing with someone is agreeing to exchange anchor measurements; it is not agreeing to probe on their behalf. A separate grant, per peer, with its own switch.
- It is **one-directional by default**. A granting B does not grant A. Reciprocity is common but must be two deliberate acts, because the operators may not be equally comfortable — one may be a hobbyist on a home connection, the other an ISP with abuse handling.
- It is **revocable at any time**, and revocation takes effect on the next request, not at the end of some window.

### 2.2 The target must be one the requester already measures publicly

This is the constraint that actually stops the relay, and it is the one I most want your opinion on.

The rule would be: B accepts to measure an address only if A already declares a **public** target for that address, protocol and port, on A's own instance — which B can verify by fetching A's public API rather than taking A's word for it.

What it buys: A cannot use B to probe anything A is not already probing, openly, under its own name. The double-check becomes strictly a second opinion on a measurement that already exists in public, which is a much narrower thing than "measure this for me".

What it costs: a private target cannot be double-checked. An operator who keeps his targets private — a legitimate choice the tool supports, and the reason the private flag exists — gets nothing from this feature.

The alternative was to accept private targets and rely on consent plus caps alone. That is the wrong trade: the verification above is cheap, mechanical, needs no human judgement, and it is the only one of the three guards that constrains *what* can be probed rather than *how much*. Better to ship the narrow version and widen it later if operators ask for it, than to ship the wide one and learn why it was a bad idea from somebody else's abuse desk.

**Decided: the target must be public on the requesting instance.** A private target cannot be double-checked, and the refusal says so plainly rather than failing obscurely. If operators later ask for private targets to be covered, it will need a different mechanism than trust — a pre-registered target list agreed between the two peers, say — and not a relaxation of this rule.

### 2.3 Caps, because consent is not a blank cheque

Even a trusted peer should not be able to turn B into a continuous prober:

| Limit | Proposed default | Why |
|---|---|---|
| Duration of one check | 15 minutes, hard maximum 1 hour | Long enough to settle an incident, too short to be a monitoring job |
| Concurrent checks per peer | 3 | An incident touches a handful of targets, not fifty |
| Checks per peer per day | 20 | A peer asking more than that is monitoring, not corroborating |
| Interval of the temporary probe | the target's own interval, floor 30 s | Nothing faster than B measures its own targets |
| Address family and port | exactly what A declares | No port sweeping through a peer |

Refusals are explicit and named, so a peer that hits a cap is told which one rather than being left to guess.

## 3. What the address may be

The existing federation safety layer already refuses to dial anything that is not a public unicast address, and that code is reused as is rather than reimplemented. Concretely, a double-check target must resolve to a global unicast address: no loopback, no RFC 1918 or unique-local, no link-local, no multicast, no unspecified address, no CGNAT range. A name is resolved and the resolved address is checked, not the string — otherwise a name pointing at 127.0.0.1 walks straight through.

This matters more here than elsewhere: B is being asked to connect somewhere on A's say-so, so the address is hostile input in a way that B's own configured targets are not.

## 4. What B measures and what happens to it

- A **temporary target**, created on B, invisible on B's public pages and absent from B's public API, carrying the requesting AS and an expiry.
- Its measurements go to a **separate table**, not into B's rollup cascade, and are purged when the check expires. They are not B's data about B's network; keeping them would be keeping somebody else's homework.
- It never appears in B's own availability figures, fault counts or status overview, for the same reason.
- It **is** visible in B's back-office, listed with the requesting AS, the address, the expiry and a stop button. Consent granted in advance does not mean invisible: an operator must be able to see at any moment what his instance is measuring for other people, and stop it.
- It is recorded in B's audit log, on grant, on request and on revocation.

## 5. What comes back

A signed report, on the existing canonical-signing scheme with the audience already bound to the recipient's AS: pass count, passes with no answer, median, p95, and the address family used. Deliberately **not** the individual passes — A does not need B's raw series to learn whether the target answers, and a summary is harder to repurpose.

On A's side the incident then reads as corroborated or contradicted:

- *B reaches it, we do not* → the fault is between A and the target, and A's own traceroute is where to look next.
- *B does not reach it either* → the target is down. Two AS, one conclusion.
- *B reaches it intermittently* → reported as such rather than forced into one of the two.

**Decided: back-office by default, publishable by a switch.** The corroboration is useful to a reader, but it publishes a fact about a third party's reachability as measured by an operator who never agreed to have it published, so it is the local operator's deliberate choice rather than a default. With the switch on, what reaches a visitor is the peer's AS, the answered-pass counts, the median and the verdict — never the check identifier, the peer URL or the raw passes.

## 6. What A learns about B, and B about A

B learns which of A's targets are in trouble, and when. Between peers who already exchange anchor measurements and approved each other by hand, that seems acceptable — but it is worth stating rather than discovering, and it is a reason the grant is per peer rather than global.

A learns nothing about B's network it could not measure itself.

## 7. Why not the obvious simpler versions

- **Ask the peer to traceroute instead of measure.** Cheaper and less dangerous, but it answers a different question: a traceroute from B tells us B's path, not whether the target answers B.
- **Use the anchors we already measure.** They say whether the path to *B* is healthy, which is useful and already displayed. It says nothing about A's target.
- **Let anyone ask any peer, rate-limited.** This is the open-relay version. No.
- **Require a human approval per request.** Defeats the purpose: the answer is needed during the incident, not after the second operator wakes up. Hence consent in advance, visible and revocable, which is what you asked for.

## 8. Rollout

The four steps were planned to be separable, and were built in one change once the two questions above were settled:

1. The grant, its switch and its back-office page.
2. The request, the caps, the address checks, the temporary target and the purge.
3. The signed report and its display in A's back-office.
4. The public wording, behind the switch of §5.

Each guard has a test that fails when that guard alone is removed, which was checked by removing each one in turn rather than assumed: consent, the public-target verification, each of the three caps, and the diversion that keeps a peer's measurements out of our own data.

## 9. Questions

**Settled**

1. ~~Require the double-checked target to be **public** on the requesting instance?~~ **Yes** (§2.2). This is what makes the feature narrow enough to build: the requester can only ever ask for a second opinion on something it already publishes under its own name.

**Still open**

2. Are the proposed caps right? (§2.3) They are a starting point, not a measurement — nobody has run this yet.
3. ~~Corroboration on A's **public** page, or back-office only to start?~~ **Back-office by default, with a switch to publish** (§5).

**Still open**

2. Are the proposed caps right? (§2.3) They shipped as proposed — 15 minutes, 3 concurrent, 20 a day, 30 s floor, with hard ceilings of 60, 10 and 50 above them. They are a starting point, not a measurement: nobody has run this at scale yet, and the first operator to hit one should say which.
4. Reciprocity: should the back-office offer "grant in return" as one click when a peer grants us, or keep the two grants entirely separate acts? Shipped as two separate acts, which is the conservative reading; a one-click reciprocal grant is an interface question, not a safety one.
