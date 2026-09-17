# Watchful fork of go2rtc

Watchful.Streaming runs go2rtc as a loopback sidecar (RTSP/HLS/MP4 in, fMP4
over WebSocket out). Upstream review has slowed (last release January 2026,
roughly 180 open PRs), so this fork carries the fixes we need ahead of
upstream and picks up community reliability and compatibility patches.

Remote naming below: `upstream` = AlexxIT/go2rtc, `origin` = Watchful-IP/go2rtc.

## Branches

- `master` — pristine mirror of upstream master. Never commit here. Sync with
  GitHub's "Sync fork" button or `git push origin upstream/master:master`.
- `watchful` — default branch and what ships. Latest upstream **release tag**
  plus carried patches, one commit per patch, so `git log v1.9.14..watchful`
  is the full diff against upstream.
- `jmw/*` — branches behind PRs opened against upstream.

## Carried patches

| Commit | Upstream | Why |
|---|---|---|
| Fix H265 RTP recovery after packet loss | [#2479](https://github.com/AlexxIT/go2rtc/pull/2479) (ours, open) | Corrupt HEVC after loss until next keyframe |
| h265: de-aggregate RFC 7798 Aggregation Packets | [#2296](https://github.com/AlexxIT/go2rtc/pull/2296) | `ffmpeg:` sources emit APs for VPS/SPS/PPS |
| h265: guard RepairAVCC against truncated AVCC | [#2419](https://github.com/AlexxIT/go2rtc/pull/2419) | Panic on empty packet kills the sidecar |
| h265: bounds safety in hvcC parsing (mpeg4.go hunk only) | [#2191](https://github.com/AlexxIT/go2rtc/pull/2191) | Panic on short hvcC from MP4/HLS sources |
| h264: wait for a partition head before depayloading | [#2380](https://github.com/AlexxIT/go2rtc/pull/2380) | Mid-NAL attach emits an undecodable keyframe |
| h264: bounds guards in GetFmtpLine | [#2193](https://github.com/AlexxIT/go2rtc/pull/2193) | Panic on malformed SPS |
| core: WriteBuffer close race | [#2339](https://github.com/AlexxIT/go2rtc/pull/2339) | Late writes into recycled http writer panic |
| mp4: guard zero timeScale | [#2192](https://github.com/AlexxIT/go2rtc/pull/2192) | +Inf timestamps from bad MP4 sources |
| tcp: Content-Length parse error handling | [#2265](https://github.com/AlexxIT/go2rtc/pull/2265) | Malformed RTSP response wedged the reader |
| tcp: RSA CBC cipher suites for older Axis | [#2383](https://github.com/AlexxIT/go2rtc/pull/2383) | rtsps to older Axis firmware fails TLS |
| tcp: RFC 7616 digest auth (SHA-256, qop) | [#2451](https://github.com/AlexxIT/go2rtc/pull/2451) | Newer Hikvision/Axis firmware rejects MD5-only |
| h265: route AP truncation through loss-recovery reset | fork-only | Keeps #2296 consistent with #2479 |
| Bump x/net, x/crypto, pion/dtls, pion/stun | replaces #2449, #2382 | Published advisories |
| Native fMP4/CMAF HLS ingest | fork-only, W-963 | H.264/HEVC/AAC, AES-128, bounded multi-track demuxing; [evidence and limits](pkg/hls/README.md) |
| streams: lock the API map access | streams half of [#2444](https://github.com/AlexxIT/go2rtc/pull/2444) | Concurrent `DELETE /api/streams` crashed the process (`concurrent map writes`); upstream closed the PR over its unrelated `app.Info` half |

## Adding a patch

```bash
git fetch upstream pull/NNNN/head:pr-NNNN
git diff $(git merge-base pr-NNNN upstream/master) pr-NNNN | git apply --3way
git commit --author="$(git log -1 --format='%an <%ae>' pr-NNNN)" \
  -m "Cherry-pick upstream #NNNN: <PR title>" \
  -m "Upstream: https://github.com/AlexxIT/go2rtc/pull/NNNN"
```

Add a row to the table above. Good candidates fix panics, leaks, or parsing
on the RTSP → fMP4 path, preserve default behaviour, and come with tests.
Skip vendor sources we don't run (Xiaomi, Nest, HomeKit, Wyze, WebRTC).

## Rebasing onto a new upstream release

```bash
git fetch upstream --tags
git rebase --onto vX.Y.Z v1.9.14 watchful   # old base → new base
```

Drop any carried commit upstream has merged, re-run the CI test set, update
the table, then release.

## Releasing

1. Set `app.Version` in `main.go` to `X.Y.Z-watchful.N` (tag builds stamp
   this exactly; branch builds append `+dev.<sha>`).
2. Tag `vX.Y.Z-watchful.N` on `watchful` and push the tag. The
   `watchful-release` workflow pushes
   `us-docker.pkg.dev/watchful-global/watchful-docker-public/go2rtc:X.Y.Z-watchful.N`
   for linux/amd64 and arm64. Every push to `watchful` also refreshes the
   `:watchful` tag for local use only. The registry is public (anonymous pull);
   the workflow authenticates through the `GCP_WORKLOAD_IDENTITY_PROVIDER` and
   `GCP_SERVICE_ACCOUNT_EMAIL` repo secrets (Infrastructure `global/wif.tf`,
   `github-go2rtc-ci`). To rebuild an existing tag with the current workflow:
   `gh workflow run watchful-release.yml --ref watchful -f ref=vX.Y.Z-watchful.N`.
3. Bump consumers: `charts/watchful-core/values.yaml` (`streaming.go2rtc.image`),
   per-environment overrides in `deployment-config`, `Watchful/src/docker-compose.yml`,
   and `ihub/apps/cli/src/commands/stream-diagnostics/playback-contract.ts`.

## Reviewed but not carried

- [#1924](https://github.com/AlexxIT/go2rtc/pull/1924) codec params from first
  keyframe. Fixes MSE aspect ratio when SPS/PPS are missing from DESCRIBE (the
  reason UniFi sources are wrapped in `ffmpeg:`), but mutates the shared codec
  without synchronisation. Needs a live UniFi test before carrying.
- [#2409](https://github.com/AlexxIT/go2rtc/pull/2409) GET_PARAMETER keepalive
  and [#2440](https://github.com/AlexxIT/go2rtc/pull/2440) handshake timeout.
  Opt-in query params; carry once `Go2RtcSourceBuilder` can set them.
- [#2327](https://github.com/AlexxIT/go2rtc/pull/2327) /
  [#2427](https://github.com/AlexxIT/go2rtc/pull/2427) keyframe-handler leaks.
  Only affect `frame.jpeg` and `stream.mp4`, which Watchful.Streaming doesn't call.
- [#2455](https://github.com/AlexxIT/go2rtc/pull/2455) credential redaction.
  Draft and conflicts with v1.9.14; revisit after the next rebase.
- [#2253](https://github.com/AlexxIT/go2rtc/pull/2253) hvc1 sample entry. Same
  change as our closed #2464; did not fix DSS playback.
- [#1943](https://github.com/AlexxIT/go2rtc/pull/1943) RTSP timestamp reset on
  reconnect. Tapo-specific heuristic with a per-packet lock.
