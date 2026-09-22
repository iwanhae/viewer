"""Viewer embedding-worker API client: claim / renew / results over bearer auth."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

import httpx


class AuthError(Exception):
    """401 — the token is wrong; fatal for the worker."""


class TransientError(Exception):
    """5xx / 429 / network trouble; worth retrying with backoff."""


class BadRequestError(Exception):
    """4xx other than 401 — the request itself is wrong; never retry."""


@dataclass(frozen=True)
class ClaimedBlob:
    hash: str
    size_bytes: int
    content_type: str
    blob_key: str
    get_url: str


@dataclass(frozen=True)
class ClaimResponse:
    embedding_dim: int
    lease_until: datetime
    claimed: list[ClaimedBlob]


@dataclass(frozen=True)
class RenewResponse:
    lease_until: datetime
    renewed: frozenset[str]


@dataclass(frozen=True)
class Rejection:
    hash: str
    reason: str


@dataclass(frozen=True)
class ResultsResponse:
    updated: int
    rejected: list[Rejection]


def _parse_time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


class ViewerClient:
    def __init__(self, base_url: str, token: str, timeout: float):
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        self._client = httpx.Client(
            base_url=base_url, timeout=timeout, headers=headers, follow_redirects=False
        )

    def claim(self, limit: int) -> ClaimResponse:
        body = self._post("/api/embedding/claim", {"limit": limit}).json()
        claimed = [
            ClaimedBlob(
                hash=item["hash"],
                size_bytes=item["sizeBytes"],
                content_type=item["contentType"],
                blob_key=item["blobKey"],
                get_url=item["getUrl"],
            )
            for item in body.get("claimed", [])
        ]
        return ClaimResponse(
            embedding_dim=body["embeddingDim"],
            lease_until=_parse_time(body["leaseUntil"]),
            claimed=claimed,
        )

    def renew(self, hashes: list[str]) -> RenewResponse:
        body = self._post("/api/embedding/renew", {"hashes": hashes}).json()
        return RenewResponse(
            lease_until=_parse_time(body["leaseUntil"]),
            renewed=frozenset(body.get("renewed", [])),
        )

    def results(self, items: list[dict]) -> ResultsResponse:
        body = self._post("/api/embedding/results", {"results": items}).json()
        return ResultsResponse(
            updated=body["updated"],
            rejected=[Rejection(r["hash"], r["reason"]) for r in body.get("rejected", [])],
        )

    def _post(self, path: str, payload: dict) -> httpx.Response:
        try:
            response = self._client.post(path, json=payload)
        except httpx.TransportError as exc:
            raise TransientError(str(exc)) from exc
        return _check(response)


def _check(response: httpx.Response) -> httpx.Response:
    if response.status_code == 401:
        raise AuthError("worker token rejected (401)")
    if response.status_code == 429 or response.status_code >= 500:
        raise TransientError(f"HTTP {response.status_code}")
    if response.status_code >= 400:
        raise BadRequestError(f"HTTP {response.status_code}: {response.text[:300]}")
    return response
