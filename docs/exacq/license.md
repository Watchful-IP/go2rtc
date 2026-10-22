# Read-only native license health

`Client.ReadLicense` implements the existing integration's non-secret license
health contract: `status`, `enterprise`, `ipCameraSlots`, and nullable
`expiresAt`. It requires an authenticated control connection and applies one
fixed operation deadline. It never changes licensing, installs a key, or
changes a recorder MAC.

## Wire requests and data handling

1. Send type 9, subtype 1 with an empty body.
2. Accept type 9, subtype 2, status zero, with exactly 22 decoded bytes. These
   are six bytes of licensed MAC followed by a 16-byte binary license key.
   Only the presence of any nonzero key byte is used. The body is cleared;
   neither the MAC nor key is retained, exposed, or logged.
3. Send type 3, subtype 1 with an empty body. Parse the type 3, subtype 2 status
   document at `/eDVR/Systems/License`. Unsolicited status updates use the same
   3/2 header and can omit Delta entirely. Skip valid XML updates without a
   license section; wait for a license-bearing report within the original
   operation deadline and cumulative 32 MiB/128-packet budget.

The status XML may itself contain `Binary` and `Mac` attributes. They are not
copied into the public result. A license-bearing report must contain exactly
one License element and every required health attribute. Ambiguous or incomplete
license reports, malformed XML/values, nonzero response status, oversized
responses, and unknown binary layouts return errors and close the failed
session. If no complete license report arrives, the timeout/message limit
returns an error. An unrelated status update is never reported as licensed.

The health attributes are `IsBadLicense`, `Trial`, `IsRestricted`, `Enterprise`,
optional `EnterpriseEx`, `MaxIPCameras`, `Expiration`, and `Time`. Time values
are Unix seconds. Expiry compares the recorder's `Time` to its `Expiration`,
avoiding host clock skew. Classification priority is invalid, expired,
unlicensed, trial, restricted, licensed. An expired trial has a null expiration
value, matching the pinned SDK's reporting. Camera limits are bounded to 65536.

The pinned SDK compares `Time >= Expiration` directly, including expiration
zero. The implementation retains that comparison for a present license key.
Permanent/non-expiring license representations have not been observed on
hardware; zero must not be interpreted as proof of a permanent license.

## Evidence

Static references are virtual addresses in the pinned Linux x64 `libevAPI.so`
1.47.27.1, whose binary identity is recorded in `auth-research.md`:

- `CConnectionImpl::LicenseRequest`, `0x26a270`: empty 9/1 request.
- `CConnectionImpl::Response`, `0x2ae0ee` and `0x2ae633`: 9/2 dispatch and
  six-byte MAC handling; `0x2ae8cd` passes body+6 into `set_binary_key`.
- `CConnectionImpl::ParseLicense`, `0x2685b0`, and its `Systems` caller around
  `0x2adcd8`: license XML is status data, not the binary 9/2 body.
- `evapi::license::is_licensed`, `0x2f1bc0`: any nonzero binary key byte.
- `evapi::license::parse`, `0x2f24e0`: attribute mapping; `0x2f269e` through
  `0x2f26ca` compare time and set expired/trial state.
- The platform's `servers/exacq-sdk/src/diagnostics.ts` maps the recovered SDK
  flags to the same public status priority and omits embedded keys.

**Hardware verified:** one pure Go connection through the lab's single forwarded
native port read a 22-byte 9/2 reply and returned `trial`, enterprise true,
four IP camera slots, expiry `2026-12-04T00:00:00Z`. The complete Connect plus
ReadLicense test took 70 ms in that run. This was a read-only query; no licensing
configuration changed. Other license states are tested with synthetic status
documents and are statically inferred, not hardware claims.

Opt-in validation: set `EXACQ_TEST_LICENSE=1`, the normal lab address and
credential-cache environment variables, and run `TestLabLicense`. It logs only
the public health fields, not raw replies.

## Interleaved status regression

A retained native session reproduced `exacq: missing or ambiguous license status`
on its twentieth health call. The failing sequence contained a successful 9/2,
then a 336-byte decoded 3/2 without Systems/License. Other observations identified
partial updates containing only Servers, Analytics or Users, with no Delta
attribute. The same interleaving made the old RefreshStatus return a Servers-only
update on 19 of 20 retained calls, omitting actual health fields.

ReadLicense now waits for the license-bearing report; RefreshStatus uses that
complete-report anchor and validates its license fields before publishing a
health snapshot. Synthetic interleaving, malformed-license, cancellation and
buffer-limit tests cover the fix. Twenty license-fix checks passed; the final
ten retained checks additionally required complete camera, storage and memory
health. A rebuilt private HTTP health call returned 200 in 30 ms with four
connections, three cameras and one storage section. This reproduces and fixes
a matching failure class; the earlier HTTP502 had no retained raw error, so its
exact cause cannot be retroactively proved.

Fresh status also reported MaxIPCameras=4 and TotalIPCameras=4 with three video
cameras and the speaker configured. Both fields describe capacity, not active
usage; the vendor client separately counts enabled devices' license consumption.
These fields alone do not establish a spare slot. See
[license-slot-research.md](license-slot-research.md) for the static evidence and
the separately coordinated capacity experiment.

## Fresh unlicensed recorder

The isolated 26.1.7.3 authentication lab returned a distinct empty status shape:
`IsRestricted`, `MaxIPCameras`, `TotalIPCameras`, `AnalogCameras` and `Expiration`
were all exactly `0`; `IsBadLicense`, `Trial`, `Enterprise`, `EnterpriseEx` and
`Time` were absent. Only this observed shape is accepted as unlicensed without
a key. Keyed replies still require the full licence fields. The status reader
accepts the shape as a complete-report anchor, without inferring a key from it.
Actual API inventory and health passed for both this server and the licensed lab.
