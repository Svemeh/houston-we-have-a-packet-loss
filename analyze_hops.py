#!/usr/bin/env python3
"""Pin each loss burst in hops.jsonl to the first hop that stopped answering.

A second counts as lost when the end target (1.1.1.1) got no reply. Its break
point is the most upstream hop from which every hop onward was also lost.
A hop that drops while hops beyond it still answer isn't where traffic died —
that's usually the hop rate-limiting ICMP to itself — so it's counted as
"isolated" and never blamed.

Bursts are matched against dishOutages.jsonl so dish-reported outages
(NO_SCHEDULE, OBSTRUCTED, ...) can be told apart from ones the dish never saw.

Usage: python3 analyze_hops.py [--hops data/hops.jsonl] [--outages data/dishOutages.jsonl] [--since 24h]
"""

import argparse
import json
import re
import sys
from collections import Counter
from datetime import datetime, timedelta, timezone

# Path order from this machine outwards, with the segment a break there blames.
# Keep in step with HopTargets in const.go.
PATH = [
    ("192.168.2.1", "LAN"),
    ("192.168.100.1", "dish"),
    ("100.64.0.1", "satellite link"),
    ("206.224.70.83", "Starlink backbone"),
    ("162.158.84.78", "Cloudflare"),
    ("1.1.1.1", "Cloudflare"),
]
END_TARGET = "1.1.1.1"
CONTROL_TARGET = "8.8.8.8"  # off-path; lost together with 1.1.1.1 -> not Cloudflare-specific

OUTAGE_MATCH_SLACK = timedelta(seconds=2)  # clock skew between dish and this machine
MAX_SAMPLE_GAP = timedelta(seconds=5)      # longer than this = logger wasn't running


def parse_time(text):
    # Go writes nanoseconds; older Pythons only take microseconds.
    text = text.replace("Z", "+00:00")
    text = re.sub(r"(\.\d{6})\d+", r"\1", text)
    return datetime.fromisoformat(text)


def parse_duration(text):
    match = re.fullmatch(r"(\d+(?:\.\d+)?)([smhd])", text)
    if not match:
        raise argparse.ArgumentTypeError("duration like 90m, 24h or 3d")
    value, unit = float(match.group(1)), match.group(2)
    return timedelta(**{{"s": "seconds", "m": "minutes", "h": "hours", "d": "days"}[unit]: value})


def read_jsonl(path):
    try:
        with open(path) as file:
            for line in file:
                try:
                    yield json.loads(line)
                except json.JSONDecodeError:
                    continue  # partial line from a crash or a write in progress
    except FileNotFoundError:
        return


def break_point(hops):
    """Index into PATH where traffic died this second, or None if the end target answered."""
    present = [(index, hops[ip]) for index, (ip, _) in enumerate(PATH) if ip in hops]
    if not present or present[-1][0] != len(PATH) - 1 or not present[-1][1]["lost"]:
        return None
    first = present[-1][0]
    for index, result in reversed(present):
        if not result["lost"]:
            break
        first = index
    return first


def label(index):
    ip, segment = PATH[index]
    return f"{segment} ({ip})"


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--hops", default="data/hops.jsonl")
    parser.add_argument("--outages", default="data/dishOutages.jsonl")
    parser.add_argument("--since", type=parse_duration, help="only look at the last e.g. 24h")
    parser.add_argument("--gap", type=int, default=2, help="answered seconds allowed inside one burst (default 2)")
    args = parser.parse_args()

    cutoff = datetime.now(timezone.utc) - args.since if args.since else None

    samples = []
    for record in read_jsonl(args.hops):
        timestamp = parse_time(record["timestamp"])
        if cutoff is None or timestamp >= cutoff:
            samples.append((timestamp, record.get("hops", {})))
    samples.sort(key=lambda sample: sample[0])
    if not samples:
        sys.exit(f"no samples in {args.hops}")

    outages = []
    for record in read_jsonl(args.outages):
        start, end = parse_time(record["start"]), parse_time(record["end"])
        if cutoff is None or end >= cutoff:
            outages.append((start, end, record))

    # ---- Per-hop totals --------------------------------------------------
    sent, lost, isolated, blamed = Counter(), Counter(), Counter(), Counter()
    for _, hops in samples:
        breakpoint_index = break_point(hops)
        for ip, result in hops.items():
            sent[ip] += 1
            if result["lost"]:
                lost[ip] += 1
        # Path hops lost while something further out still answered.
        for index, (ip, _) in enumerate(PATH):
            result = hops.get(ip)
            if result and result["lost"] and (breakpoint_index is None or index < breakpoint_index):
                isolated[ip] += 1
        if breakpoint_index is not None:
            blamed[PATH[breakpoint_index][0]] += 1

    # ---- Bursts ----------------------------------------------------------
    bursts = []  # each: list of (timestamp, hops, breakpoint_index)
    current, last_loss_index, previous_time = [], None, None
    for sample_index, (timestamp, hops) in enumerate(samples):
        logger_gap = previous_time is not None and timestamp - previous_time > MAX_SAMPLE_GAP
        previous_time = timestamp
        if current and (logger_gap or sample_index - last_loss_index > args.gap + 1):
            bursts.append(current)
            current = []
        breakpoint_index = break_point(hops)
        if breakpoint_index is not None:
            current.append((timestamp, hops, breakpoint_index))
            last_loss_index = sample_index
    if current:
        bursts.append(current)

    span = samples[-1][0] - samples[0][0]
    print(f"{len(samples)} samples, {samples[0][0]:%Y-%m-%d %H:%M:%S} → {samples[-1][0]:%Y-%m-%d %H:%M:%S} ({span})")
    print(f"{len(bursts)} loss bursts to {END_TARGET}, {len(outages)} dish outages\n")

    burst_segments, matched_outages = Counter(), set()
    burst_causes = Counter()
    if bursts:
        print("BURSTS")
        for burst in bursts:
            start, end = burst[0][0], burst[-1][0] + timedelta(seconds=1)
            breaks = Counter(entry[2] for entry in burst)
            # Most common break point; ties go to the more upstream hop.
            primary = min(breaks, key=lambda index: (-breaks[index], index))
            burst_segments[PATH[primary][1]] += 1

            mix = ""
            if len(breaks) > 1:
                mix = "  mix: " + ", ".join(f"{PATH[index][0]}×{count}" for index, count in sorted(breaks.items()))
            control_lost = sum(1 for _, hops, _ in burst if hops.get(CONTROL_TARGET, {}).get("lost"))

            overlapping = [
                (outage_index, record)
                for outage_index, (outage_start, outage_end, record) in enumerate(outages)
                if outage_start - OUTAGE_MATCH_SLACK <= end and outage_end + OUTAGE_MATCH_SLACK >= start
            ]
            matched_outages.update(outage_index for outage_index, _ in overlapping)
            if overlapping:
                outage_text = ", ".join(f"{record['cause']} {record['duration_s']:.1f}s" for _, record in overlapping)
                burst_causes[overlapping[0][1]["cause"]] += 1
            else:
                outage_text = "none"
                burst_causes["(no dish outage)"] += 1

            print(
                f"  {start.astimezone():%Y-%m-%d %H:%M:%S}  {len(burst):>3}s lost  "
                f"{label(primary):<34} {CONTROL_TARGET} lost {control_lost}/{len(burst)}  "
                f"dish outage: {outage_text}{mix}"
            )
        print()

    print("PER HOP                sent     lost    loss%   broke here   isolated")
    for ip in [ip for ip, _ in PATH] + [CONTROL_TARGET]:
        if not sent[ip]:
            continue
        print(
            f"  {ip:<18} {sent[ip]:>8} {lost[ip]:>8} {100 * lost[ip] / sent[ip]:>7.3f}% "
            f"{blamed[ip] if ip in dict(PATH) else '-':>12} {isolated[ip] if ip in dict(PATH) else '-':>10}"
        )
    print("  broke here = lost seconds where this hop and everything beyond it went silent")
    print("  isolated   = lost while a hop further out answered (usually ICMP rate limiting, not real loss)\n")

    if bursts:
        print("BURSTS BY SEGMENT")
        for segment, count in burst_segments.most_common():
            print(f"  {segment:<20} {count:>5}  ({100 * count / len(bursts):.0f}%)")
        print("\nBURSTS BY DISH OUTAGE CAUSE")
        for cause, count in burst_causes.most_common():
            print(f"  {cause:<20} {count:>5}  ({100 * count / len(bursts):.0f}%)")

    unmatched = [record for index, (_, _, record) in enumerate(outages) if index not in matched_outages]
    if outages:
        print(f"\nDish outages with no matching burst: {len(unmatched)}/{len(outages)}")
        for cause, count in Counter(record["cause"] for record in unmatched).most_common():
            print(f"  {cause:<20} {count:>5}")


if __name__ == "__main__":
    main()
