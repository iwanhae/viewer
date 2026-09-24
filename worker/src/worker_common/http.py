"""Bearer-authenticated viewer worker API client."""

from __future__ import annotations

import httpx


class AuthError(Exception):
    """The worker token was rejected."""


class TransientError(Exception):
    """A network or server failure that can be retried."""


class BadRequestError(Exception):
    """A non-retryable request error."""


def check(response: httpx.Response) -> httpx.Response:
    if response.status_code == 401:
        raise AuthError("worker token rejected (401)")
    if response.status_code == 429 or response.status_code >= 500:
        raise TransientError(f"HTTP {response.status_code}")
    if response.status_code >= 400:
        raise BadRequestError(f"HTTP {response.status_code}: {response.text[:300]}")
    return response


class ViewerClient:
    def __init__(self, base_url: str, token: str, timeout: float):
        self._client = httpx.Client(
            base_url=base_url,
            timeout=timeout,
            headers={"Authorization": f"Bearer {token}"} if token else {},
            follow_redirects=False,
        )

    def _post(self, path: str, payload: dict) -> httpx.Response:
        try:
            response = self._client.post(path, json=payload)
        except httpx.TransportError as exc:
            raise TransientError(str(exc)) from exc
        return check(response)
