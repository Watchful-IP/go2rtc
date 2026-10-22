# Control API errors and diagnostics

Every `POST /api/exacq` failure returns JSON with a stable `code`, a secret-free
`message` that says what to fix, the `phase` that failed, and whether a retry can
help. Callers branch on `code`; `message` is for people.

```json
{"error":{"code":"auth_invalid_credentials","message":"recorder rejected the username or password",
  "phase":"login","retryable":false,"recorderStatus":-3,
  "recorder":{"serial":"00-15-5D-03-49-00","version":"26.1.7.3"}}}
```

`recorderStatus` is the signed native packet status, present only when the
recorder sent one. `recorder` is present once discovery (serial) or configuration
(version) succeeded. Messages are redacted of the password, proxy URL and its
userinfo, PCM, webhook URL and authorization; the username is also removed from
`internal` and `protocol_error` messages, which may quote an unclassified cause.

| Code | HTTP | Retry | Meaning |
| --- | --- | --- | --- |
| `invalid_request` | 400 (405/415 for method/content type) | no | Request shape or argument is wrong; message names it |
| `camera_not_found` | 404 | no | Camera, stream or audio output is not on this recorder |
| `config_conflict` | 409 | no | A managed event link or webhook target was edited on the recorder |
| `busy` | 409 | yes | Another apply for the same recorder owner is running |
| `auth_invalid_credentials` | 422 | no | Wrong username or password (status -3, or SPAKE proof mismatch) |
| `auth_no_accounts` | 422 | no | Recorder has no user accounts (status -2) |
| `auth_schedule_blocked` | 422 | no | Login blocked by the user's schedule (status -4) |
| `auth_directory_failed` | 422 | no | LDAP/Kerberos login failed (status 1-3) |
| `auth_unsupported` | 422 | no | Enterprise/OAuth-only or no supported SPAKE mechanism |
| `auth_rejected` | 422 | no | Any other nonzero login status |
| `permission_denied` | 422 | no | User may not search recordings, events or read configuration |
| `stream_unavailable` | 422 | no | Stream disabled, unbound, rejected or silent (camera sends no video), or licence invalid (phase `license`) |
| `unsupported_codec` | 422 | no | Not H.264/H.265 video, or not G.711 talkdown output |
| `no_recording` | 422 | no | No decodable recording/keyframe in the requested interval |
| `webhook_unsupported` | 422 | no | Recorder lacks EventPI webhook POST; message names its version |
| `config_rejected` | 422 | no | Recorder refused a configuration write; often missing Setup permission |
| `unreachable` | 502 | yes | Refused, no route, DNS failure, or EdgeLink SOCKS reply naming the cause |
| `proxy_failed` | 502 | yes | EdgeLink proxy itself unreachable or refused the route credentials |
| `not_exacq` | 502 | no | Port answered with HTTP/TLS/SSH or sent no greeting within 5 s |
| `protocol_error` | 502 | no | Unexpected or oversize recorder response |
| `connection_lost` | 502 | yes | Recorder closed the connection mid-operation |
| `decoder_unavailable` | 503 | no | FFmpeg missing on the go2rtc host |
| `timeout` | 504 | yes | Dial or operation exceeded its deadline |
| `config_not_persisted` | 504 | yes | Write accepted but fresh logins did not show it within 15 s |
| `internal` | 500 | no | Unclassified failure; please report |

The P-224 and SPAKE2+ exchanges reject a wrong password by failing the server
proof, without a recorder status: the 26.1.7.3 lab returns
`auth_invalid_credentials` with no `recorderStatus` for a bad password.

## Request diagnostics

- `X-Request-Id` is echoed, or generated when absent or not printable ASCII.
- `Server-Timing` lists cumulative `dial`, `handshake`, `login`,
  `configuration` (and `license` for diagnose), `action` and `total` in ms,
  summed across every session the request opened.
- One `[exacq] control` log line per request (module `exacq`): request id, action,
  recorder host:port, proxied, serial, version, HTTP status, outcome code,
  connect count, total and per-phase ms. 4xx log at debug, 5xx at warn.

## `diagnose`

A read-only connection test. HTTP 200 for any recorder-side outcome; it stops at
the first failing step and attaches the same error object to it.

```json
{"ok":true,"recorder":{"serial":"00-15-5D-03-49-00","version":"26.1.7.3"},"steps":[
 {"step":"dial","ok":true,"ms":1,"detail":"connected directly to 192.168.3.232:22610"},
 {"step":"handshake","ok":true,"ms":46,"detail":"exacqVision Server 00-15-5D-03-49-00 answered; login uses SPAKE (P-224)"},
 {"step":"login","ok":true,"ms":8,"detail":"authenticated"},
 {"step":"configuration","ok":true,"ms":8,"detail":"exacqVision 26.1.7.3; 3 cameras; webhook POST supported"},
 {"step":"license","ok":true,"ms":4,"detail":"licence trial"},
 {"step":"webhook","ok":true,"ms":0,"detail":"webhook POST notifications available"}]}
```

An invalid, expired or unlicensed licence fails the `license` step; a recorder
without webhook POST fails the final `webhook` step after everything else passed.

## Webhook plan/apply

`webhook.plan` uses one session. `webhook.apply` uses no throwaway session: one
worker session plans and writes, then fresh-login readbacks verify at 200 ms,
400 ms, 800 ms, 1.6 s then every 2 s within the 15 s budget. Both also remove
this owner's legacy log-only motion links left by the earlier event-log design. Plans and apply results include
`changes`, the secret-free mutations in write order, for example
`create callback target`, `update callback URL`, `rotate callback authorization`,
`add camera 459264`, `remove camera 459265`, `repair event link for camera 459264`,
`remove legacy log-only link for camera 459264`, `remove callback target`.

```json
{"configured":true,"verified":true,"create":2,"update":0,"delete":1,
 "links":[{"eventId":42,"cameraId":459264}],
 "changes":["create callback target","add camera 459264","remove legacy log-only link for camera 459264"]}
```

Read-only lab check: `EXACQ_TEST_ADDRESS=<host:port> EXACQ_TEST_CREDENTIALS=<item.json>
go test ./internal/exacq -run TestLabControlErrors -v` runs diagnose, health, a
plan for an unused owner, a wrong password, the Web Service port and a closed port.
