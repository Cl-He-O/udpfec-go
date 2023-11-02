This is a tool for reducing packet lost designed for gaming purpose.

Neither encryption nor authentication are implemented as the intended use case - tunneling udp-based proxy/VPN - already include those security features.

Obfuscation is planned.

## Config

Examples are given with good default settings. However, you could tweak them as you need.

- "timeout": maximum time between receiving the first packet in the packet group and sending the FEC packets.
- "group_live": duration for a packet group to live after last receiving when decoding
- "fec": number at index i, starting from 1, represents the number of parity packets to send for i incoming packets.

Note: "fec" params must be the same for client and server.
