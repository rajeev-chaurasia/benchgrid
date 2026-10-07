#!/usr/bin/env python3
"""gpubench: a perception-style GPU workload for benchgrid rigs with an NVIDIA GPU.

A small residual convolutional network over a batch of camera-sized frames in
fp16, the shape of work a perception stage does every cycle. Weights and
inputs come from a fixed seed and the algorithms are forced deterministic, so
the output, and the checksum printed after it, are the same on every run on
the same GPU and software; the agent fails a run whose checksum changes.

Timing uses CUDA events around the timed forward passes only, so it measures
GPU time, not Python overhead or host-device synchronisation outside them.
"""

import argparse
import hashlib
import os
import sys

# Deterministic cuBLAS needs this before CUDA initialises. The agent gives a
# benchmark a clean environment, so the script sets it itself.
os.environ.setdefault("CUBLAS_WORKSPACE_CONFIG", ":4096:8")

import torch  # noqa: E402
import torch.nn as nn  # noqa: E402


class Block(nn.Module):
    def __init__(self, c: int):
        super().__init__()
        self.a = nn.Conv2d(c, c, 3, padding=1, bias=False)
        self.b = nn.Conv2d(c, c, 3, padding=1, bias=False)
        self.na, self.nb = nn.BatchNorm2d(c), nn.BatchNorm2d(c)

    def forward(self, x):
        y = torch.relu(self.na(self.a(x)))
        return torch.relu(x + self.nb(self.b(y)))


def model(width: int, depth: int) -> nn.Module:
    layers = [nn.Conv2d(3, width, 7, stride=2, padding=3, bias=False), nn.BatchNorm2d(width), nn.ReLU()]
    c = width
    for stage in range(3):
        layers += [Block(c) for _ in range(depth)]
        if stage < 2:
            layers += [nn.Conv2d(c, c * 2, 3, stride=2, padding=1, bias=False), nn.BatchNorm2d(c * 2), nn.ReLU()]
            c *= 2
    layers += [nn.AdaptiveAvgPool2d(1), nn.Flatten(), nn.Linear(c, 64)]
    return nn.Sequential(*layers)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--batch", type=int, default=8)
    p.add_argument("--size", type=int, default=256)
    p.add_argument("--width", type=int, default=32)
    p.add_argument("--depth", type=int, default=2)
    p.add_argument("--iters", type=int, default=20)
    a = p.parse_args()

    if not torch.cuda.is_available():
        print("no CUDA device", file=sys.stderr)
        return 3
    torch.manual_seed(1234)
    torch.backends.cudnn.benchmark = False
    torch.backends.cudnn.deterministic = True
    torch.use_deterministic_algorithms(True)

    dev = torch.device("cuda")
    net = model(a.width, a.depth).to(dev).half().eval()
    gen = torch.Generator(device="cpu").manual_seed(42)
    x = torch.randn(a.batch, 3, a.size, a.size, generator=gen).to(dev).half()

    with torch.inference_mode():
        out = net(x)  # first pass: allocation and kernel selection, not timed
        torch.cuda.synchronize()
        start, end = torch.cuda.Event(enable_timing=True), torch.cuda.Event(enable_timing=True)
        start.record()
        for _ in range(a.iters):
            out = net(x)
        end.record()
        torch.cuda.synchronize()
    ms = start.elapsed_time(end)

    frames = a.batch * a.iters
    digest = hashlib.sha256(torch.round(out.float() * 1000).to(torch.int64).cpu().numpy().tobytes()).hexdigest()[:16]
    print(f"BENCHGRID_METRIC gpu_time_ns {int(ms * 1e6)}")
    print(f"BENCHGRID_METRIC frames_per_s {frames / (ms / 1000):.3f}")
    print(f"BENCHGRID_CHECKSUM {digest}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
