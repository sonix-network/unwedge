#!/usr/bin/env python3
# Temporary diagnostic: a dig-equivalent raw DNS probe using only the stdlib,
# for hosts where dig/nslookup aren't installed. Queries the system resolvers
# (from /etc/resolv.conf) and a couple of public resolvers for the AAAA/A of
# $HOST, and prints the response RCODE + answers per server. The queried name
# is never printed (it may come from a secret); only the resolved addresses are.
import os
import socket
import struct

RCODES = {0: "NOERROR", 1: "FORMERR", 2: "SERVFAIL", 3: "NXDOMAIN",
          4: "NOTIMP", 5: "REFUSED"}
QTYPE = {1: "A", 28: "AAAA"}


def build_query(qname, qtype):
    header = struct.pack(">HHHHHH", 0x1234, 0x0100, 1, 0, 0, 0)  # RD set
    body = b""
    for label in qname.rstrip(".").split("."):
        body += struct.pack("B", len(label)) + label.encode("idna")
    body += b"\x00" + struct.pack(">HH", qtype, 1)  # qtype, class IN
    return header + body


def parse(resp):
    _, flags, qd, an, _, _ = struct.unpack(">HHHHHH", resp[:12])
    off = 12
    for _ in range(qd):  # skip question section
        while resp[off] != 0:
            off += 1 + resp[off]
        off += 5  # zero label + qtype + qclass
    answers = []
    for _ in range(an):
        if resp[off] & 0xC0 == 0xC0:
            off += 2
        else:
            while resp[off] != 0:
                off += 1 + resp[off]
            off += 1
        rtype, _, _, rdlen = struct.unpack(">HHIH", resp[off:off + 10])
        off += 10
        rdata = resp[off:off + rdlen]
        off += rdlen
        if rtype == 28 and rdlen == 16:
            answers.append(socket.inet_ntop(socket.AF_INET6, rdata))
        elif rtype == 1 and rdlen == 4:
            answers.append(socket.inet_ntop(socket.AF_INET, rdata))
    return flags & 0xF, an, answers


def query(server, qname, qtype, label):
    fam = socket.AF_INET6 if ":" in server else socket.AF_INET
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.settimeout(3)
    try:
        s.sendto(build_query(qname, qtype), (server, 53))
        resp, _ = s.recvfrom(4096)
        rcode, an, answers = parse(resp)
        print("  %-34s rcode=%-8s answers=%d %s"
              % (label, RCODES.get(rcode, rcode), an, answers))
    except Exception as e:
        print("  %-34s ERROR %s: %s" % (label, type(e).__name__, e))
    finally:
        s.close()


def resolv_nameservers():
    ns = []
    try:
        with open("/etc/resolv.conf") as f:
            for line in f:
                line = line.strip()
                if line.startswith("nameserver"):
                    ns.append(line.split()[1])
    except OSError:
        pass
    return ns


def main():
    host = os.environ["HOST"]
    print("Probing for the controller host (name masked). qtypes: AAAA, A")
    for ns in resolv_nameservers():
        query(ns, host, 28, "system %s AAAA" % ns)
        query(ns, host, 1, "system %s A" % ns)
    for pub, name in [("2606:4700:4700::1111", "cloudflare"),
                      ("2001:4860:4860::8888", "google")]:
        query(pub, host, 28, "%s AAAA" % name)


if __name__ == "__main__":
    main()
