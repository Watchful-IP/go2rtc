# Existing repository test failures

The unrelated full-suite failures were independently reproduced from a clean
`git archive HEAD` of baseline commit `f43aa70` (`Prepare go2rtc
1.9.14-watchful.6 release`). The archive was extracted outside all working
trees, at `/tmp/exacq-native-baseline-review`. It contains none of the native
Exacq changes. No parent working tree was modified.

On the same macOS host and Go toolchain, these commands fail on that baseline:

```sh
go test ./internal/tuya ./internal/ffmpeg ./pkg/hap/... ./pkg/mdns/...
go test ./pkg/tuya/...
```

| Package | Reproduced baseline failure |
|---|---|
| `internal/ffmpeg` | H.265 expected command lines use `-level:v 5.1`, while implementation uses `-x265-params level=5.1:high-tier=0`; device tests also expect Windows dshow arguments on macOS avfoundation. |
| `pkg/hap/camera` | Test refers to undefined `SetupEndpoints`. |
| `pkg/hap/hds` | Tests refer to undefined `Client`. |
| `pkg/hap/tlv8` | `TestVideoCodecParams` expected value mismatch. |
| `pkg/mdns` | `TestDiscovery` fails with `no interfaces for listen` in this environment. |
| `pkg/tuya` | Vet rejects nonconstant format strings passed to `fmt.Errorf` at `cloud_api.go:211` and `:257`. |

The raw local results are `/tmp/exacq-native-baseline-tests.log` and
`/tmp/exacq-native-baseline-tuya.log`. These tests were not disabled or changed
to make the Exacq work pass. Targeted Exacq tests, its race-enabled package
tests, and shared stream redaction regression tests are validated separately.
