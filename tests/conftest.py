"""Shared test fixtures."""

from collections.abc import AsyncIterator
from pathlib import Path
from unittest.mock import patch

import pytest

from .fake_agent import FakeAgent


@pytest.fixture(autouse=True)
def auto_enable_custom_integrations(enable_custom_integrations):
    """Let Home Assistant load the custom integration in every test."""


@pytest.fixture(autouse=True)
def fast_reconnect() -> None:
    with patch("custom_components.hostbeacon.connection.BACKOFF_START", 0.05), patch(
        "custom_components.hostbeacon.connection.BACKOFF_MAX", 0.2
    ):
        yield


@pytest.fixture
async def agent(tmp_path: Path, socket_enabled: None) -> AsyncIterator[FakeAgent]:
    fake = FakeAgent(tmp_path)
    await fake.start()
    yield fake
    await fake.stop()
