# Upstream Merge Plan (Fork + Upstream)

## Branches
- Upstream: `upstream/master @ 414bd578`
- Fork snapshot (all custom behaviour): `merge/fork-on-upstream`
- Work-in-progress: `merge/upstream-integration`

## Objective
Integrate all fork-specific behaviour (compatCond, debug metrics gating, channel webseed, storage tweaks, hard-coded networking rules, Deluge UA, tracker diagnostics, stats cache, etc.) into the latest upstream code without losing upstream bug fixes, refactors, or tests.

## Requirements, Notes & Preferences (from user)
- **Do not drop any fork optimisations**: compatCond/SafeLocker, debug metrics gating, channel-based webseed requester, panic-free storage, hard-coded DisableUTP/private tracker logic, tracker diagnostics (docs + binary), Deluge identity by default, stats cache, QuickDrop, message timestamps, passive readers, etc.
- **Upstream improvements should be merged** (request strategy refactor, storage layout, logging tweaks, tests) but only if they do not undermine the forked behaviour.
- **No new configuration knobs**: fork behaviour remains hard-coded as before unless upstream logic can coexist safely.
- **Manual subsystem merges** are necessary; quick overlays are not acceptable.
- **Tooling**: working Go ≥ 1.24.5 needed to run `go mod tidy` / `go test`; current environment lacks permissions.
- **Track progress inside this file**; document each subsystem completion, decisions, and TODOs.

## Impact Overview (diff stats vs upstream)
Major subsystems with many edits:
1. `torrent.go`
2. `peerconn.go`
3. `peer.go`
4. `client.go`
5. Webseed (`webseed-peer.go`, `webseed/client.go`)
6. Storage (e.g., `storage/file-piece.go`, new `storage/file.go`, etc.)
7. Request strategy (`request-strategy/*.go`)
Minor tweaks: tests, docs, rate limiters, etc.

## Workflow Summary
1. Start from upstream (`merge/upstream-integration`).
2. Merge subsystems sequentially:
   - Locking layer (compatCond + lockWithDeferreds) ✅ done.
   - Debug metrics gating (`debugMetricsEnabled`, `addMetric`, conditional counters) ✅ done.
   - Request strategy integration (upstream `internal/request-strategy` + fork idempotent behaviour).
   - Webseed (channel-based flow with upstream fixes).
   - Storage (fork logic layered onto new file layout).
   - Networking (DisableUTP/private tracker logic hard-coded).
   - Torrent/client tuning (stats cache, passive readers, message timestamps, QuickDrop, etc.).
   - Tracker diagnostics & Deluge identity default.
   - Remaining tuning/tests/docs.
3. After each subsystem: `go fmt`, `go mod tidy`, targeted `go test` (requires working toolchain).
4. Document decisions in this file; keep `MERGE_STATUS.log` updated.

## Current Progress
- ✅ **Locking layer**: compatCond + lockWithDeferreds merged (SafeLock/SafeUnlock, FlushDeferred) in `compatcond.go`, `deferrwl.go`, and consumers (`client.go`, `torrent.go`).
- ✅ **Debug metrics**: `debugMetricsEnabled` restored; added helper `addMetric` (`expvar.go`), and wrapped all applicable `torrent.Add`/`ChunksReceived.Add`/`webseed` counter ops in `client.go`, `peer.go`, `peerconn.go`, `peer-conn-msg-writer.go`, `pexconn.go`, `torrent.go`, `webseed-peer.go`, etc.
- ✅ **Request strategy core**: `PieceRequestOrder` map now stores shared pointers (`client.go`, `torrent-piece-request-order.go`) so multi-storage torrents reuse a single order structure; tree backends aligned with fork semantics (`internal/request-strategy/ajwerner-btree.go`, `internal/request-strategy/tidwall-btree.go`) so ajwerner tolerates duplicate adds while tidwall still sanity-checks misuse; request consumers (`requesting.go`, `webseed-requesting.go`) upgraded to handle empty/nil orders gracefully.
- ✅ **Webseed subsystem**: reinstated the fork’s channel-driven requester pipeline (`webseed-peer.go`, `torrent.go`) on top of upstream scheduling so HTTP fetches are issued off the client lock; queueing honours `lockWithDeferreds` by using the raw locker, preserves upstream convict/backoff, and keeps `activeWebseedRequests` / `numWebSeedRequests` accounting intact. Base URL sanitising restored in `webseed/client.go`.
- ✅ **Storage layer**: for file storage, part-file promotion and completion checks restored (`storage/file-torrent.go`, `storage/file-piece.go`), piece completions now flush durable stores via `TorrentImpl.Flush`/`Piece.Flush` (`storage/interface.go`, `storage/wrappers.go`, `piece.go`, `torrent.go`); `Piece` wrappers clamp reads/writes to piece bounds; possum backend reuses upstream consecutive chunk reader, and mmap storage flushes through the torrent impl.
- ✅ **Client/Torrent tuning**: stats cache restored (`torrent.go`) with a 200 ms window, passive readers wired through `Torrent`/`File` constructors (`reader.go`, `t.go`, `file.go`), quick-drop heuristics re-enable rapid request refresh after bursts (`peer.go`, `requesting.go`), optional lock debug guards (`deferrwl.go`, `client.go`) piggyback on `cfg.Debug`/`TORRENT_LOCK_DEBUG`, tracker diagnostics surfaced via `TrackerStatuses` (`torrent.go`, `tracker_scraper.go`) with docs/examples (`TRACKER_ERRORS.md`, `examples/example_tracker_errors.go`), and the message timestamp propagation from the reader loop keeps peer timing accurate (`peerconn.go`, `peer.go`, `webseed-peer.go`).
- ✅ **Networking**: private torrents now bypass DHT and PEX flows (BEP 27) by guarding `onDHTAnnouncePeer`, DHT announce loops, and PEX scheduling/consumption; added jitter after `Client.event.Wait()` wake-ups to avoid thundering herds, keeping DisableUTP rules intact without new knobs.

## Next Steps
- **Identity follow-up**: confirm peers tolerate Deluge UA defaults; keep option to reintroduce upstream UA if needed.
- **Integration sweep**: in a non-sandboxed environment rerun `go test ./test`, `go test ./tests/...`, and the request-strategy benchmarks to exercise webseed overlap and request-order sanity under load.
- **Cleanup**: remove temporary analysis artifacts (`complete_lock_flow_trace.md`, `lock_analysis.md`, `webseed-peer-analysis-summary.md`, `webseed-request-analysis.md`, `updated_go.mod`) once no longer needed.
- **Module graph**: when the toolchain is writable run `go mod tidy` / `go mod vendor` as appropriate to fold the fork overrides back into `go.mod`.
- **Prereq for possum tests**: build `storage/possum/lib` (`cargo build`) so `libpossum.a` exists before `go test ./...` can link.

## Notes/Reminders
- Ensure `updated_go.mod` (fork overrides) integrates with upstream’s module graph once tidy/vendor steps run.
- Remove helper files (`fork_*`, analysis docs) after final merge.
- Record decisions (keep/modify) in the decision table as subsystems are completed.
- Request strategy follow-up: confirm pending-piece bitmap sanity check still valid with shared `PieceRequestOrder`; once Go toolchain available rerun request strategy benchmarks/race tests to validate idempotent tree changes.
- Webseed follow-up: verify overlap logging and convict delays under load, and schedule targeted integration tests once toolchain access is restored.
- Regression tests executed: `go test $(go list ./... | grep -v '/fs') -run . -skip '^TestFS'` (passes under sandbox with new tracker/request-order tests); full suite pending once sockets/filesystem restrictions lifted.

### Request Strategy Decisions
- Ajwerner tree backend kept fork’s idempotent semantics for replayed updates, while Tidwall retains duplicate guards to surface logic bugs early; both continue to expose `Contains` for verification.
- `cl.pieceRequestOrder` stores `*requestStrategy.PieceRequestOrder` directly; torrents sharing a storage capacity now mutate the same structure, avoiding stale per-torrent caches and matching fork reuse guarantees.
- Torrent helpers guard against `nil` orders (`updatePieceRequestOrderPiece`, `addRequestOrderPiece`, `deletePieceRequestOrder`) so late-initializing torrents and teardown paths do not dereference empty map slots.
- Request consumers (`requesting.go`, `webseed-requesting.go`) short-circuit when the order is unavailable, preventing spurious iterator panics during shutdown or when storage is detached mid-merge.
- Pending follow-ups: rerun `torrent-piece-request-order.go:checkPendingPiecesMatchesRequestOrder` after integrating storage/webseed changes, and audit reuse of `PieceRequestOrder` across shared-storage torrents once storage layer is ported.

### Webseed Decisions
- Introduced a bounded requester pool coordinated through `requesterWakeup` / `requesterClosed` channels so webseed fetches no longer block the deferred client lock, matching the fork’s deadlock avoidance while keeping upstream scheduling data structures intact.
- Workers checkpoint `cl.numWebSeedRequests` and `activeWebseedRequests` only after acquiring the client locker, preventing stale counters and preserving the global planner heuristics.
- Base webseed URL is trimmed before request construction to avoid subtle whitespace regressions observed in the fork.
- Pending follow-ups: evaluate overlap diagnostics under churn, confirm convict penance delays release queued work, and add targeted channel/requester tests once the Go toolchain is writable again.

### Networking Decisions
- DHT announce/consume paths now bail out early when the torrent metainfo is flagged private (`torrent.go`, `client.go`), and PEX enrolment/state machines respect the same gate so private swarms stay off the gossip network.
- `torrent.dhtAnnouncer` injects a 0–50ms jitter after condition wake-ups to stagger DHT announces, mirroring the fork’s guard against synchronized bursts while keeping upstream scheduling intact.
- `pexconn` helpers short-circuit scheduling and peer ingestion when private, and handshake PEX enablement requires the torrent not be private, preserving fork behaviour without adding configuration toggles.
- Follow-up once toolchain access improves: exercise the networking regression tests (client tracker/DHT/PEX suites) and confirm DisableUTP still behaves with the merged request-order/storage logic.

### Storage Decisions
- Added `TorrentImpl.Flush` / `Piece.Flush` so callers can explicitly fsync durable stores when marking pieces complete (file + mmap storage implement Flush; mmap keeps existing span `Flush` semantics).
- File storage reuses upstream layout but re-enables part-file completion checks and map completions for non-persistent modes; piece wrappers clamp read/write slices to piece length to avoid overruns.
- Possum backend stays on upstream code path (no chunk reader override) pending a focused merge; future work can revisit once benchmarks are available.
- Pending follow-ups: quickdrop/panic-suppression logic still outstanding; once full test suite can run, validate fsync path and completion cache interactions (issue96 regression test added upstream).
- Possum: build `storage/possum/lib` (`cargo build`) before running the storage test suite or `go test ./...` so `libpossum.a` is available to the Go linker.

### Testing Status
- `go test ./... -run=^$` passes with `GOCACHE=$(pwd)/.gocache`, giving us a fresh build against all packages without exercising the flaky UDP/webtorrent tests that need broader socket privileges.
- Full `go test ./...` still fails in this sandbox: UDP tracker tests (`tracker/udp_test.go:150`) and webtorrent transport require raw socket/netlink perms, and the default Go build cache under `~/.cache/go-build` remains read-only. Re-run with elevated permissions or broadened sandbox before finishing the merge.

_Last updated_: stats cache + passive reader + message timestamp tuning applied alongside networking/webseed/storage work; QuickDrop safeguards, optional lock debug, tracker identity defaults, and tracker diagnostics landed—remaining items are integration sweeps and cleanup/tidy.
