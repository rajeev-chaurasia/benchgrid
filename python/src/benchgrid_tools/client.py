"""A minimal client for the benchgrid API, standard library only, so the gate
can run in any CI image with Python and nothing installed but numpy."""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

TERMINAL = {"SUCCEEDED", "FAILED", "INVALID"}


class APIError(RuntimeError):
    pass


@dataclass
class Client:
    base: str
    timeout: float = 30.0

    def _call(self, method: str, path: str, body: bytes | None = None, content_type: str = "application/json") -> bytes:
        req = urllib.request.Request(self.base.rstrip("/") + path, data=body, method=method)
        if body is not None:
            req.add_header("Content-Type", content_type)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            raise APIError(f"{method} {path}: {e.code} {e.read()[:300]!r}") from e

    def upload_blob(self, data: bytes) -> str:
        return json.loads(self._call("POST", "/v1/blobs", data, "application/octet-stream"))["sha256"]

    def submit(self, spec: dict, key: str, max_attempts: int = 3) -> str:
        body = json.dumps({"spec": spec, "idempotency_key": key, "max_attempts": max_attempts}).encode()
        return json.loads(self._call("POST", "/v1/experiments", body))["id"]

    def experiment(self, exp_id: str) -> dict:
        return json.loads(self._call("GET", f"/v1/experiments/{exp_id}"))

    def wait(self, exp_id: str, timeout: float = 1800, poll: float = 1.0) -> dict:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            e = self.experiment(exp_id)
            if e["state"] in TERMINAL:
                return e
            time.sleep(poll)
        raise TimeoutError(exp_id)

    def samples(self, exp_id: str, attempt: int, metric: str) -> list[float]:
        """The measured samples of one metric, warmups excluded, read from the
        sealed run the experiment's final attempt produced."""
        raw = self._call("GET", f"/v1/artifacts/runs/{exp_id}/attempt-{attempt}/samples.jsonl")
        out = []
        for line in raw.splitlines():
            s = json.loads(line)
            if s["metric"] == metric and not s["warmup"]:
                out.append(float(s["value"]))
        return out
