"""Tests for setting up the Hostbeacon Integration."""

from homeassistant.core import HomeAssistant
from homeassistant.setup import async_setup_component

from custom_components.hostbeacon.const import DOMAIN


async def test_setup(hass: HomeAssistant) -> None:
    """The Integration loads in Home Assistant."""
    assert await async_setup_component(hass, DOMAIN, {})
    assert DOMAIN in hass.config.components
