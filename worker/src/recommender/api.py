"""Viewer embedding-worker API client: claim / renew / results over bearer auth."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

from worker_common.http import AuthError, BadRequestError, TransientError, ViewerClient as BaseClient


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


class ViewerClient(BaseClient):

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
