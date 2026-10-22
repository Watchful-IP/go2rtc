# Recorder-originated event callbacks

The recorder's EventPI webhook action is separate from WebService. A short
authenticated native connection reads configuration and provisions the callback;
the recorder then makes outbound HTTP POST requests without an open go2rtc
subscription. The destination must be reachable **from the recorder**. Video and
configuration still use the configured native forwarded port; callback delivery
uses the receiver's existing HTTP ingress.

The vendor documents target URLs, routes, custom headers, certificate validation,
and event-link association in [Configuring a webhook connection](https://docs.johnsoncontrols.com/exacq/r/Exacq/en-US/exacqVision-Client-User-Manual/24.03/System-configuration/Notifications-window/Webhooks-tab/Configuring-a-webhook-connection).
Its default batch interval is five seconds. This integration preserves the
recorder's global interval and does not promise subsecond event delivery.

## Native configuration contract

`Client.PlanWebhook(WebhookConfig)` returns safe change counts and the exact
owned event-link-to-camera mapping. `Client.ConfigureWebhook(ctx, config)`
reconciles the desired camera list and confirms persistence on fresh native
sessions. Every temporary session closes on completion or failure. Empty
`CameraIDs` removes only this owner's webhook event links and target.

Mutation is serialized per recorder address, proxy route, and owner within a Go
process, including fresh planning and persistence checks. Waiters honor context
cancellation; the registry retains only hashed keys and removes idle entries,
with at most 1,024 active owner entries. Multiple go2rtc instances need external
single-owner coordination because the recorder assigns IDs to new event links.

`WebhookConfig` contains `OwnerID`, `CameraIDs`, full callback `URL`, and an
`Authorization` header. The URL must be HTTP(S), without user information or a
fragment. Target and route IDs are deterministic SHA-256-derived UUIDs; event
association IDs use a separate owner namespace plus the canonical camera ID.
Legacy log-only motion links use a different namespace; webhook plan/apply
removes this owner's legacy links in the same reconciliation.

The route is POST with a five-second timeout and the provided authorization
header. URL, header, and recorder webhook secrets remain in unexported native
snapshot fields; inventory and plan JSON do not include them. Unfamiliar owned
actions, extra routes/headers, duplicate associations, or another event referring
to the owned target cause a conflict rather than being overwritten.

The SDK's `UpdateWebhookTarget` at `0x2ac680` builds an ordinary configuration
write (`4/3`). `Config::ToUpdateRequestXML` at `0x365ba0`,
`Target::ToXML` at `0x363fe0`, and `Route::ToXML` at `0x363ca0` establish:

```xml
<eDVR Delta="1">
  <Webhooks ID="655360">
    <Target ID="TARGET-UUID" Host="https://receiver.example"
            TrustCert="0" DefaultTimeoutMs="5000" Digest="" Secret="">
      <Route ID="ROUTE-UUID" Path="/callback" Method="POST" TimeoutMs="5000">
        <Header Name="Authorization" Value="Bearer PRIVATE-TOKEN"/>
      </Route>
    </Target>
  </Webhooks>
</eDVR>
```

`655360` is the observed EventPI ID, not a hardcoded assumption: implementation
uses the recorder's Webhooks and Modules IDs and requires them to agree.
The native event action (`ev::EventTarget::Write`, `0x228c60`; SDK target enum10)
is nested beneath its motion event:

```xml
<Action><Webhooks ID="655360"><Target ID="TARGET-UUID">
  <Route ID="ROUTE-UUID"/>
</Target></Webhooks></Action>
```

Motion sources use the existing native Devices/Device/Video/Input CASID mapping.
Owned links have zero pre/post alarm delay, logging enabled, and all 168 weekly
hours enabled with `AlarmEnabled="3"` (the recorder normalizes `1` to `3` when
logging is enabled). They do not alter recording schedules or
physical camera encoding. Target creation precedes link creation; cleanup removes
links before the target, with a fresh readback proving link removal before the
target deletion is sent. Event deletion includes the association and zero-delay
attributes used by the SDK; an ID-only delete was ignored by the recorder.
Failed persistence verification never blindly retries a
create. Configuration is not a recorder transaction, so callers may rerun a fresh
plan to inspect partial completion. Plan and apply also delete this owner's legacy log-only
motion links and report each mutation in `changes`; verification readbacks back
off from 200 ms to 2 s (see [errors.md](./errors.md)).

Recorder behaviours verified on 26.1.7.3 (22 October 2026):

- A changed `Value` for an existing route header is silently ignored. Updates
  therefore delete `Authorization` and set the new value in the same write; the
  recorder applies both together, so rotation has no unauthenticated gap. `Host`
  and `Path` changes apply in place.
- Any event configuration write briefly re-arms the event engine. A motion that
  starts within a few seconds of a write may deliver only its end
  (`alarmed: false`), which is not alerted. Steady-state triggers delivered the
  start every time.
- Footage is archived a few seconds after the event, about as late as the
  batched callback. Event-time snapshots therefore poll the recorded edge every
  500 ms, up to 12 s past the three-second selection window, before failing with
  `no_recording`.

## Verification boundary

Unit tests cover initial planning, fresh-shape idempotence, authorization rotation,
cleanup ordering, exact event/camera mapping, unrelated-object preservation,
ownership conflicts, invalid URL/header/camera rejection, and public JSON secrecy.
Affected native package and private API race suites pass. The coordinated real
recorder run established the target and exact event/camera mapping using pure Go.
The receiver accepted natural event batches after all native TCP sessions had
closed. Two owners were exercised: removing the first owner's link left the
second owner's verified link and target intact, and final second-owner removal
returned an empty owned-link set. Two simultaneous real private-API apply requests
against a clean owner both returned the same single verified event-link ID;
cleanup then returned no owned links. This qualifies the process-local mutation
guard against the recorder's server-assigned ID behavior, in addition to the
unit barrier and cancellation tests.

That run exposed two server normalizations now covered by regressions: logged
alarm schedules read back as `3`, and an ID-only event delete is ignored. The
initial orphaned test link was recovered with the full association-bearing delete;
subsequent cleanup verifies link removal before deleting the destination.

Callback delivery is not evidence of durable queueing or replay after receiver
outages. Incoming motion `source.type` uses the core enum (`2` in the observed
payload), not the SDK enum (`5`). Route using the fresh plan's exact event-link
mapping; a callback's event ID is a link identity, not a globally unique event
occurrence. Payload and transport failure qualification is recorded separately.

## Delivery semantics

The real motion payload uses `source.type=2`, a canonical camera `source.id`,
an event-link `id`, an `alarmed` boolean, and a UTC `ts` with fractional seconds.
Only alarm starts become motion alerts. The exact provisioned event-link/camera
mapping validates callbacks; event time is retained for historical snapshots.

Delivery is intentionally best-effort. The recorder's five-second batch setting
is preserved. One controlled trigger arrived about 2.07 seconds after the trigger
request; this is a single batching observation, not a latency percentile. Six
batches deliberately received HTTP 500. No identical rejected batch was retried
over roughly 139 seconds of observation, including recovery to HTTP 200.
Static EventPI inspection likewise found queue removal before HTTP delivery and
no application-level requeue. Failed deliveries may be lost. No integration
retry queue, history backfill, or persistent event connection compensates for
that loss; ordinary platform webhook processing remains responsible for accepted
messages.

The compiled NVR auto-configuration test created an owned target/link, observed a
fresh no-op plan, accepted a real recorder callback, and obtained a 63,772-byte
historical JPEG through the pure-Go API with zero SDK providers. NVR and VMS
compiled replay tests also obtained historical images from an actual captured
callback. The initial tests exercised an in-memory scheduler fixture; they do
not qualify a production queue deployment. The final simplified NVR and VMS builds each replayed the captured callback
through ordinary parse/snapshot hooks and obtained a 64,089-byte event-time JPEG,
with zero delivery jobs and a stable event ID. No custom deduplication store or
delivery retry queue remains. The shared companion suite passed 121 tests
(5 opt-in tests skipped), and the integration typecheck passed.
Final recorder configuration matched the original snapshot after ignoring XML
element order and recorder clock values. All test targets and links were removed;
physical camera settings and recording schedules were unchanged.
