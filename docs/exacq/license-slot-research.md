# Disabled devices and camera license slots

Read-only static analysis of Exacq 26.1 on 2026-10-21 establishes that the
vendor client's available-license calculation excludes disabled devices.
Disabling an existing speaker in place is therefore a supported candidate
for temporarily freeing its consumed slot without deleting its identifiers.
The server's actual release and reuse of that slot has **not** been verified
by this analysis; no device or license setting was changed.

`TotalIPCameras` is a capacity, **not the number of currently enabled devices**.
A successful disable can leave both `MaxIPCameras` and `TotalIPCameras`
unchanged. Inventory enabled states and successful ordinary use of a newly
available slot are the relevant observations. No license data, MAC address,
or capacity field should be changed.

## Client evidence

Binary: `linux/usr/local/exacq/client/edvrclient`

SHA-256: `f4342c710f1c27b8fd93ccc255d552a4ebd604c0bd199fef112807320f26f7fc`.

Addresses below are ELF virtual addresses; names come from the binary's
symbol table, not inferred C callback layouts.

| Address | Symbol / finding |
| --- | --- |
| `0x1283a60` | `SystemImpl::CountEnabledIPCameras() const` iterates `Devices()` and sums each device's virtual `GetLicenseCount()` result. |
| `0x1283ac0` | `SystemImpl::CountEnabledIPCameraLicenses(unsigned long&, unsigned long&) const` obtains the same counts and splits them into two license categories. |
| `0xc00730` | `DeviceImpl::GetLicenseCount() const`: an empty IP address returns zero; otherwise it calls `getEnabled()` and returns zero when false. |
| `0xc007de`–`0xc00803` | Enabled check. The optimized implementation reads byte `this+0x1c8`, or calls the override at vtable slot `+0x2f0`. |
| `0xc0f780` | `DeviceImpl::getEnabled() const`, the default implementation compared at that call site. |
| `0xc00890`–`0xc008d6` | Disabled/empty-address cleanup and zero return, before connection status or the stored license-count fallback is consulted. |
| `0x1284360` | `SystemImpl::NumAvailableIPCameraLicenses(CameraLicenseType) const` subtracts enabled-license counts from the corresponding capacities, clamping negative availability to zero. |
| `0xf0b60c`–`0xf0b63b` | `License::ProcessLicenseStatus` reads `MaxIPCameras` into license object offset `+0x1c`. |
| `0xf0b63e`–`0xf0b66d` | The same parser reads `TotalIPCameras` into offset `+0x20`. |
| `0x128439d`–`0x1284400` | Availability uses `MaxIPCameras`, `max(MaxIPCameras, TotalIPCameras)`, or their difference depending on license category, then subtracts the separately computed enabled counts. |

Virtual-table resolutions independently confirm the calls:

- `DeviceImpl` table `0x17c0fa8`, address point `+16`, slot `+0x570`:
  `DeviceImpl::GetLicenseCount` at `0xc00730`.
- `SystemImpl` table `0x1850bd8`, address point `+16`, slot `+0x160`:
  `SystemImpl::CountEnabledIPCameraLicenses` at `0x1283ac0`.
- `SystemImpl::Devices() const` at `0x12411d0` returns the vector at
  `this+0x2220`; the zero count comes from the device's enabled check, rather
  than assuming that this vector already contains only enabled devices.

This establishes the vendor client's counting contract. It does not establish
how quickly the recorder applies an enabled-state update or prove that every
device type consumes exactly one slot.

## Server serialization cross-check

Binary: `server/usr/local/exacq/server/plugins/streampi.so`

SHA-256: `ba826b563770bf067be8285cd6a6d14a2645b06de96f9ea0ce409c33364a09ab`.

The stripped function beginning at `0xe95c0` serializes the license status.
At `0xe9623`–`0xe9639`, `MaxIPCameras` comes from byte `license+0xc`;
at `0xe963e`–`0xe9654`, `TotalIPCameras` comes from byte `license+0xd`.
Both are guarded by the same license-valid byte at `+8`. These fields are
not computed by counting the current inventory at this serialization point.
The client interpretation above provides the stronger evidence that they
represent capacities.

The server's enforcement path is not established by this bounded inspection.
Any subsequent hardware observation should distinguish that evidence from
the static client contract recorded here.
