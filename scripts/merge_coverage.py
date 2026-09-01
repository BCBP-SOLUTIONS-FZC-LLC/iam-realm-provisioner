#!/usr/bin/env python3
"""Merge multiple Go coverage profiles, taking the max count per block.
This ensures a block covered in any suite is shown as covered in the merged output.

Usage: merge_coverage.py <profile1> [<profile2> ...] > coverage.out
"""
import sys
from collections import defaultdict

counts = defaultdict(int)
for fname in sys.argv[1:]:
    try:
        for line in open(fname):
            line = line.strip()
            if line.startswith('mode:') or not line:
                continue
            # Format: pkg/file.go:startLine.startCol,endLine.endCol numStmts count
            parts = line.rsplit(' ', 2)
            if len(parts) != 3:
                continue
            key = parts[0] + ' ' + parts[1]
            try:
                n = int(parts[2])
            except ValueError:
                continue
            counts[key] = max(counts[key], n)
    except FileNotFoundError:
        pass

print('mode: atomic')
for k, n in sorted(counts.items()):
    print(k, n)
