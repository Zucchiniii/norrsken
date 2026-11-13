# Third-party software

Norrsken bundles or links the following. Their licenses apply to those parts.

| Component | Used for | License |
|---|---|---|
| [wireguard-go](https://git.zx2c4.com/wireguard-go) (`golang.zx2c4.com/wireguard`) | Userspace WireGuard (NordLynx) | MIT |
| [netlink](https://github.com/vishvananda/netlink) and [netns](https://github.com/vishvananda/netns) | Routes, addresses, links | Apache-2.0 |
| [Go x/sys, x/net, x/crypto](https://cs.opensource.google/go/x) (incl. `x509roots/fallback`) | System calls, crypto, fallback CA roots | BSD-3-Clause |
| [rsc.io/qr](https://github.com/rsc/qr) | QR code on the pairing screen | BSD-3-Clause |
| [Inter](https://github.com/rsms/inter) | UI font (`app/assets/fonts`) | SIL Open Font License 1.1 (`app/assets/fonts/OFL-Inter.txt`) |
| Go standard library | Everything else in the daemon | BSD-3-Clause |

`app/js/countries-fallback.js` is a snapshot of NordVPN's public server-country list, used only
when the live list can't be fetched.

NordVPN and NordLynx are trademarks of Nord Security. Norrsken is not affiliated with or endorsed
by Nord Security.
