"""Offline Linux x64 libevAPI slice-type oracle; invoke with `uv run python`.

No EVAPI session is initialized and no recorder or camera is contacted. This
checks codec classification only, never native timestamp semantics.
"""
import argparse
import ctypes as C
import hashlib
import json
import pathlib
import re


parser = argparse.ArgumentParser()
parser.add_argument("--library", type=pathlib.Path, required=True)
parser.add_argument("--fixtures", type=pathlib.Path, required=True)
args = parser.parse_args()
digest = hashlib.sha256(args.library.read_bytes()).hexdigest()
assert digest == "14bd776c0fb0c46b72f2d2034a04ea2fa6a5794d0876facb1fc20cf4bda13509"
library = C.CDLL(str(args.library.resolve()))
base = C.cast(library.EVAPI_Init, C.c_void_p).value - 0x36AF80
is_b_frame = C.CFUNCTYPE(C.c_bool, C.c_int, C.c_void_p, C.c_uint32)(base + 0x1A7120)

results = []
for name, codec, aud, pattern in [
    ("b.h264", 0x85, 9, "IPBBPBBPBBPBBPB"),
    ("b-closed.h265", 0x86, 70, "IPBBBPBBBPBBBPB"),
]:
    data = (args.fixtures / name).read_bytes()
    starts = [m.start() for m in re.finditer(b"\x00\x00\x00\x01" + bytes([aud]), data)]
    starts.append(len(data))
    expected = [kind == "B" for kind in pattern * 3]
    assert len(starts) - 1 == len(expected)
    observed = []
    for begin, end in zip(starts, starts[1:]):
        # The SDK helper expects the four-byte start codes used by captured
        # native access units; x264 emits mixed three/four-byte start codes.
        nalus = [nalu for nalu in re.split(b"\x00\x00\x00?\x01", data[begin:end]) if nalu]
        payload = b"".join(b"\x00\x00\x00\x01" + nalu for nalu in nalus)
        buffer = C.create_string_buffer(payload)
        observed.append(bool(is_b_frame(codec, buffer, len(payload))))
    results.append({
        "fixture": name,
        "frames": len(observed),
        "expectedB": sum(expected),
        "sdkB": sum(observed),
        "allClassificationsMatch": observed == expected,
    })

print(json.dumps({"librarySHA256": digest, "results": results}, indent=2))
