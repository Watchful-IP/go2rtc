# Recorder-backed camera groups

On 22 October 2026, the actual Exacq 26.1.7.3 lab recorder was tested with three
temporary ordinary groups. The vendor SDK created and removed the fixture;
all qualification reads, media requests and motion writes used the pure Go
client through the private API and compiled ihub VMS integration. No SDK
provider or Web Service was configured in that integration runtime.

| Fixture | Direct camera members | Expected platform behavior |
| --- | --- | --- |
| Parent | Axis Q1728 | Listed; returns only Q1728 |
| Nested child | Axis P1468 and Dahua | Listed separately; returns those two cameras |
| Nested audio-only child | None; C1310 output item | Excluded from camera-group choices; no cameras |

All expectations passed. Native IDs, parent IDs and direct memberships matched
fresh independent SDK readback. The parent does not recursively union its child
cameras: this preserves the existing platform group contract. The synthetic
All Cameras selection retained three cameras and one standalone speaker, while
a nonexistent selected group returned no devices. Camera source URLs used the
native `exacq` scheme; parent and child snapshots returned real JPEG images.

The compiled integration applied motion logging for two distinct test owners
using the groups' actual camera lists. Repeated planning reported configured.
Disabling the first owner preserved the second owner's recorder links and
scheduled registration. Both were then disabled and fresh native planning
verified cleanup. The scheduler was an explicit in-memory fixture; this test
does not establish Redis durability or new end-to-end alarm delivery evidence.

## Restoration and reproduction

Only the three temporary groups and exact dedicated-owner logging links were
modified. Every group mutation used a fresh SDK session. After deleting the
children and then their parent, independent SDK readback matched the original
group tree, four devices, camera settings, recording schedules, codecs and
licence state. A final pure Go configuration read showed zero groups, with
the complete Events and Users sections equal to their saved baselines.

Sanitized results are in `group-validation.json`. The opt-in companion test is
`integrations/exacqvision-http-shared/wire-groups.device.test.ts`; it requires an
explicit private fixture file and owns only its dedicated motion-link identities.
Fixture provisioning and private recovery snapshots are retained outside Git.

The SDK's `EVAPI_NumGroups` enumerates roots only; independent inspection needed
`EVAPI_GetChildGroupId` to traverse children. The pure Go parser already recurses.
The separate SDK product inventory currently lacks that traversal and was not
changed as part of this pure Go qualification. Map groups, cross-server members
and every vendor group-item class remain outside this recorded lab proof.
