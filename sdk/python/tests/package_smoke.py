"""Run with an isolated interpreter after installing a wheel or sdist, not source."""

import asyncio
from importlib.metadata import distribution

import httpx

from mango_sdk import AsyncMango, Mango, __version__, models
from mango_sdk._generated import OPERATIONS

package = distribution("mango-sdk")
assert package.version == __version__
assert package.locate_file("mango_sdk/py.typed").is_file()
assert len(OPERATIONS) == 98
assert models.Agent


def handle(request: httpx.Request) -> httpx.Response:
    assert request.method == "POST"
    assert request.url.path == "/proxy/v1/agents"
    assert request.headers["Authorization"] == "Bearer package-test-only"
    assert request.headers["User-Agent"] == f"mango-sdk-python/{__version__}"
    return httpx.Response(200, json={"id": "agent_package_test"})


with Mango(
    base_url="https://mango.invalid/proxy", api_key="package-test-only",
    transport=httpx.MockTransport(handle),
) as client:
    assert client.create_agent(body={"name": "test", "model": "test"})["id"] == "agent_package_test"


async def main() -> None:
    async with AsyncMango(
        base_url="https://mango.invalid/proxy", api_key="package-test-only",
        transport=httpx.MockTransport(handle),
    ) as client:
        result = await client.create_agent(body={"name": "test", "model": "test"})
        assert result["id"] == "agent_package_test"


asyncio.run(main())
print(f"Installed mango-sdk {__version__}: sync/async requests, 98 operations, and typing marker verified")
