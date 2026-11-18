"""Minimal UDP sender and listener for the broadcast and multicast checks.

  udp.py listen <port> [<group> <local-ip>]   prints the number of datagrams received in 4 s
  udp.py send <dst> <port> [<local-ip>]       sends 5 datagrams
"""
import socket
import struct
import sys
import time


def listen(port, group=None, local=None):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("", port))
    if group:
        mreq = struct.pack("4s4s", socket.inet_aton(group), socket.inet_aton(local))
        s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, mreq)
    s.settimeout(0.2)
    n, end = 0, time.time() + 4
    while time.time() < end:
        try:
            s.recv(2048)
            n += 1
        except socket.timeout:
            pass
    print(n)


def send(dst, port, local=None):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 1)
    if local:
        s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, socket.inet_aton(local))
        s.bind((local, 0))
    for _ in range(5):
        s.sendto(b"vxlan-cni test", (dst, port))
        time.sleep(0.2)


if __name__ == "__main__":
    mode, args = sys.argv[1], sys.argv[2:]
    if mode == "listen":
        listen(int(args[0]), *args[1:3])
    else:
        send(args[0], int(args[1]), *args[2:3])
