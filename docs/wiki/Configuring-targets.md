# Configuring targets, and adjusting them

A target that is badly sized produces noise: loss that is not loss, a median
that mixes machines, red bars that mean nothing. This page explains how to
size a target, how to recognise the three usual kinds of noise, and what to
change.

---

## The four settings

| Setting | What it does | Sensible values |
|---|---|---|
| **Interval** | Time between two passes | 60 s for most things, 30 s for a link you watch closely, 300 s for a distant or fragile target |
| **Packets** | How many probes per pass | 10 is a good default; 20 gives finer percentiles but hits rate limiters harder |
| **Spacing** | Delay between two packets of a pass | 200 to 300 ms. Below 100 ms you are testing the target's rate limiter |
| **Timeout** | How long a reply is still counted | 1000 ms on a national path, 2000 ms intercontinental |

A burst must fit in its interval, with margin: `packets × spacing + timeout`
must stay under 75 % of the interval. smokestack refuses a target that does
not respect it.

**The default worth knowing**: 10 packets spaced by 200 ms, every 60 s. It is
gentle on the target, gives 600 measurements an hour, and is what the
ready-made targets use.

### What the number of packets changes

Loss is a ratio, so the number of packets sets its resolution:

| Packets per pass | One lost packet reads as |
|---|---|
| 5 | 20 % loss on that pass |
| 10 | 10 % |
| 20 | 5 % |
| 50 | 2 % |

With 20 packets, a single lost packet draws a visible red bar, while the
15-minute figure may stay at 0.7 %. **The bar is not the story, the average
is.** If a target loses one packet here and there and you find the bars
distracting, lower the packet count rather than distrust the tool.

---

## The three kinds of noise, and what to do

### 1. The target rate-limits ICMP

**What you see.** A small, steady loss — a fraction of a percent to a few
percent — while `ping` from the same machine shows nothing. Very common
towards home routers, ONT, CPE, and towards public "proof" or test hosts.

**How to confirm**, from the server running the probe:

```sh
ping -c 100 -i 0.2 <target>     # the pattern smokestack uses: a burst
ping -c 100 -i 1   <target>     # one packet per second
```

Loss on the first and none on the second: the destination is limiting its
replies. Nothing is wrong with your network.

**What to change.** 10 packets spaced by 300 ms. The median and the
percentiles stay just as good; only the sensitivity to the limiter drops.

### 2. The name points at several machines

**What you see.** Loss in round fractions — 33 %, 50 %, 66 % — with an
interval that divides the 15-minute window. With a 5-minute interval, that
window holds three passes: one silent server out of three reads as 33 %.

**Why.** Names like `pool.ntp.org`, `fr.pool.ntp.org` or
`outlook.office365.com` resolve to a different machine every few minutes.
Each pass measures whichever server DNS returned, in a different place and
under a different load, and the ones ignoring ICMP look like packet loss.

**smokestack tells you.** A target whose name answered from several addresses
carries a notice in the back-office, and its public page explains it to
visitors. The addresses seen in the last 24 hours are listed.

**What to change.** Press **Pin** to freeze the address actually measured, or
better, target a specific machine. For time servers, prefer a named
university server over a pool: the ready-made targets offer several.

### 3. The path really loses packets

**What you see.** Loss that both `ping` patterns reproduce, or that appears
at a given time of day, or on one address family only.

**That one is worth keeping.** This is what the tool is for. A traceroute is
taken automatically, compared with the last healthy path, and the hop that
changed or went silent is highlighted.

---

## Reading a target that fails completely

The back-office shows the reason under the target's name:

| Message | Meaning |
|---|---|
| `cannot resolve …` | The name has no address in the requested family. A target forced to IPv6 whose name has only an A record fails here |
| `IPv6 is not available on this probe` | The host has no IPv6, or the probe could not open an ICMPv6 socket |
| `no reply to N ICMP echo requests sent to …` | The packets left, nothing came back. The message gives the address actually probed |

---

## Suggested settings by kind of target

| Target | Interval | Packets | Spacing | Timeout |
|---|---|---|---|---|
| Your transit providers, your exchanges | 60 s | 10 | 200 ms | 1000 ms |
| A link you are actively troubleshooting | 30 s | 10 | 200 ms | 1000 ms |
| Public resolvers, anycast services | 60 s | 10 | 200 ms | 1000 ms |
| A home router, an ONT, a CPE | 300 s | 10 | 300 ms | 1500 ms |
| An intercontinental destination | 60 s | 10 | 300 ms | 2000 ms |
| A TCP service (HTTPS, SSH, BGP) | 60 s | 5 | 500 ms | 2000 ms |

TCP targets open a real connection each time: keep the packet count low, and
prefer a service you are allowed to knock on.

---

## Should this be automatic?

smokestack deliberately does not retune a target on its own. Changing the
packet count changes the resolution of the loss figure, and changing the
interval changes the shape of the history: a graph that silently retunes
itself is no longer comparable with its own past, which is precisely what a
one-year history is for.

What it does instead is name what it sees — rate limiting, a rotating name, a
path that really loses — and offer the one-click change, so the decision stays
yours and is recorded.

---

## Being warned when it matters

*My targets alerting* in the back-office sends an alert when one of your
targets stays in incident for longer than you choose (5 minutes by default).
A traceroute is taken the moment the incident opens, and its path, compared
with the last healthy one, travels in the message. There is a silence window
per target so a flapping link does not wake anybody every hour, and an
optional notice when it recovers.

Alerts leave through the channels of *Notification channels*: email with your
own SMTP settings or the local sendmail, a webhook, Slack, Teams, Telegram,
Twilio (SMS and WhatsApp), OVHcloud SMS, GatewayAPI. Chat and SMS get a
shortened message; email carries the traceroute. Test each channel with the
button next to it.

Alerting has two switches: the global one on that screen, and one per target
(*Alerts* in the target list). A target left out is still measured and its
incidents still recorded — you simply are not woken up for it. Useful for a
target known to be noisy, a test target, or a destination whose owner you do
not control.
