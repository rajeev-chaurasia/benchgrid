# 0005. Emulated rigs are opt in and always labelled

## Decision

An agent started with a profile that overrides any hardware fact advertises
`emulated: true`, and every run it produces carries that flag. A spec matches
an emulated rig only if it sets `allow_emulated`.

## Why

This repository has no physical rig and no GPU. Its rigs are processes on one
host, some advertising hardware they do not have so that capability matching
and gating can be exercised. That is a legitimate way to test a scheduler and
an illegitimate way to produce a performance number about the hardware being
imitated. The flag makes the difference impossible to lose: a gate on real
hardware cannot be satisfied by an emulated rig, and no consumer of a run can
mistake one for the other.
