#!/usr/bin/env python3
"""Raw L2 component probe; invoke in an explicitly selected test netns."""
import argparse
import collections
import json
import socket
import struct
import sys
import time


def checksum(data):
    if len(data) % 2:
        data += b"\x00"
    total = sum(struct.unpack("!%dH" % (len(data) // 2), data))
    while total >> 16:
        total = (total & 65535) + (total >> 16)
    return (~total) & 65535


def frame(mac, kind, marker, arp_target):
    source = bytes.fromhex(mac.replace(":", ""))
    dest = b"\xff" * 6
    if kind == 0x86DD:
        dest = bytes.fromhex("333300000001")
        src = socket.inet_pton(socket.AF_INET6, "fd00:504::11")
        dst = socket.inet_pton(socket.AF_INET6, "ff02::1")
        nd = struct.pack("!BBHI", 136, 0, 0, 0x20000000) + src
        nd += bytes([2, 1]) + source + bytes([253, 1]) + marker
        pseudo = src + dst + struct.pack("!I3xB", len(nd), 58)
        value = checksum(pseudo + nd)
        nd = nd[:2] + struct.pack("!H", value) + nd[4:]
        payload = struct.pack("!IHBB", 6 << 28, len(nd), 58, 255) + src + dst + nd
    elif kind in (0x0806, 0x8035):
        opcode = 1 if kind == 0x0806 else 3
        payload = struct.pack("!HHBBH", 1, 0x0800, 6, 4, opcode)
        payload += source + (socket.inet_aton("172.16.176.60") if kind == 0x0806 else b"\x00" * 4)
        target = marker if kind == 0x0806 else source
        payload += target + (socket.inet_aton(arp_target) if kind == 0x0806 else b"\x00" * 4) + marker
    else:
        payload = marker + b"\x00" * 40
    packet = dest + source + struct.pack("!H", kind) + payload
    return packet + b"\x00" * max(0, 60 - len(packet))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("listen", "send"))
    parser.add_argument("--device", required=True)
    parser.add_argument("--marker", default="OPR123")
    parser.add_argument("--source")
    parser.add_argument("--arp-target", default="172.16.176.61")
    parser.add_argument("--seconds", type=float, default=10)
    parser.add_argument("--types", default="88b5,0806,86dd,8035")
    args = parser.parse_args()
    marker = args.marker.encode("ascii")
    if len(marker) != 6:
        parser.error("marker must contain exactly six ASCII bytes")
    with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3)) as conn:
        conn.bind((args.device, 0))
        if args.mode == "send":
            if not args.source:
                parser.error("send requires --source")
            for kind in args.types.split(","):
                conn.send(frame(args.source, int(kind, 16), marker, args.arp_target))
            print(json.dumps({"sent": args.types, "source": args.source}))
            return
        print("READY", file=sys.stderr, flush=True)
        conn.settimeout(0.2)
        deadline = time.monotonic() + args.seconds
        observed = collections.Counter()
        while time.monotonic() < deadline:
            try:
                packet, address = conn.recvfrom(65535)
            except socket.timeout:
                continue
            if address[2] == socket.PACKET_OUTGOING or marker not in packet or len(packet) < 14:
                continue
            source = ":".join("%02x" % byte for byte in packet[6:12])
            kind = "%04x" % struct.unpack("!H", packet[12:14])[0]
            observed[source + "/" + kind] += 1
        print(json.dumps(dict(observed), sort_keys=True))


if __name__ == "__main__":
    main()
