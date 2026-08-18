# OCI Descriptor-Closure Verification — P2P Transfer Experiment

A hardware-free experiment supporting [harbor-satellite#542 — LFX Mentorship
Term 3: Air-Gapped Peer-to-Peer OCI Image
Distribution](https://github.com/container-registry/harbor-satellite/issues/542).
It demonstrates why a **tag-first naive copy** of an OCI artifact is unsafe
when content is being pulled from a peer, and how a **digest-addressed,
closure-aware** transfer engine satisfies the integrity and predictable-failure
requirements described in that issue.

**Demo video:** [Watch the demo](https://drive.google.com/file/d/1cddB_osgdjlRfOnXYwNgiM-zy5f1iwxy/view?usp=drive_link)

Three local registry containers stand in for a small trusted-peer topology:

| Container | Role                                    |
|-----------|------------------------------------------|
| `peerA`   | Primary peer, seeded with a full artifact |
| `peerC`   | Fallback peer                             |
| `local`   | The "pulling" Satellite's own registry (Zot; falls back to `registry:2` if Zot config is uncooperative) |

The artifact under test is a small **multi-arch image** copied in with
`crane` — an index → two platform manifests → configs + layers, an ~8-node
descriptor graph. Full closure digests are recorded before any test runs.

## Quickstart

```bash
docker compose up -d
./scripts/seed.sh
./scripts/run-demos.sh | tee results/run.log
```

## What this proves

Four demonstrations, run in priority order:

1. **The violation (naive copy).** One platform child manifest is deleted
   from `peerA` after seeding. A tag-first naive copy publishes the root
   index locally *before* discovering the hole, so `crane pull --platform
   linux/arm64` against `local` fails while `linux/amd64` succeeds — a
   broken artifact is now live locally. This is exactly the failure mode
   the invariant below forbids.

   <img width="1912" height="782" alt="Image" src="https://github.com/user-attachments/assets/6f231a6a-cd63-4708-b47e-58a253475f9b" />

2. **The invariant (this harness).** A small Go program (`~150–200` lines,
   built directly on `go-containerregistry`) walks the descriptor graph and
   fetches + digest-verifies **leaves → children → root**, publishing the
   root manifest only after the full closure is verified. Killing `peerA`
   mid-blob-transfer means the root never appears locally; rerunning against
   `peerC` completes successfully and `crane validate` passes.

   <img width="1917" height="718" alt="Image" src="https://github.com/user-attachments/assets/e53725bb-c7ec-4279-866e-f61f9085c630" />

3. **Corruption.** A single byte is flipped in a blob on the peer. The
   digest mismatch is caught and nothing is published.

   <img width="1916" height="795" alt="Image" src="https://github.com/user-attachments/assets/96ba18a4-4420-4484-b56d-19e4020d9217" />

4. **Concurrency.** Two simultaneous requests for the same digest are
   collapsed via singleflight into a single transfer.

   <img width="1917" height="770" alt="Image" src="https://github.com/user-attachments/assets/63a48c98-1ddb-4712-9aac-84b71fa0be8d" />

Every run is captured with `tee`; the pasted output under `results/` is the
evidence — no GPU or special hardware required to reproduce any of this.

## Repo layout

```
.
├── docker-compose.yml       # peerA, peerC, local registries
├── scripts/
│   ├── seed.sh              # builds & pushes the multi-arch test artifact
│   └── run-demos.sh         # runs demos 1–4 in order, tee'd to results/
├── internal/
│   └── walker/               # descriptor-graph walk + digest verification
├── cmd/
│   └── harness/               # the ~150–200 line transfer harness
└── results/
    └── run.log               # captured output from the last run
```

## Design notes

- **Digest-addressed, not tag-addressed.** Peers are never asked about tags;
  a tag is resolved once, at the desired-state source, and everything after
  that is content-addressed. This avoids the tag-mutation problem entirely.
- **Root-visible-last.** The root manifest is only published locally once
  every node in its descriptor closure has been fetched and verified.
- **Content addressing separates integrity from trust.** Wrong bytes are
  caught by digest verification regardless of which peer sent them; the
  trusted-peer list only governs *availability and reachability* decisions,
  never correctness.

## Non-goals (v1)

Dynamic peer discovery (mDNS/DHT), gossip protocols, untrusted peers, WAN
distribution, and any changes to artifact signing are explicitly out of
scope for this experiment — matching the non-goals listed in #542 (no DHT
or swarm protocol, no untrusted-peer federation, no Ground Control changes).

## Relationship to #542's acceptance criteria

This experiment is a small, self-contained proof for a subset of the
acceptance criteria in #542, ahead of any full implementation:

- **Digest verification before local availability** — demo 2 (the
  invariant) and demo 3 (corruption) show the root manifest is only
  published after every referenced blob is fetched and digest-verified.
- **Predictable failure on missing/unreachable peers** — demo 2's
  mid-transfer kill test shows the harness fails cleanly against `peerA`
  and completes against `peerC`, without leaving local registry state
  corrupted.
- **No corruption of local registry state** — demo 1 shows what happens
  *without* this invariant (a broken artifact goes live), as the baseline
  the harness is built to avoid.

Peer eligibility checks, retry/backoff configuration, and the full
multi-Satellite e2e topology are out of scope for this experiment and are
left to the proposal's implementation milestones.
