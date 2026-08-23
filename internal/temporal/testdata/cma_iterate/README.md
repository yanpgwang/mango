# Iterate fixture

This fixture is adapted from Anthropic's `claude-cookbooks` Managed Agents
"Iterate: do -> observe -> fix" example. It is intentionally tiny: `calc.py`
contains two planted defects and `test_calc.py` describes the expected
behavior. Mango uses it only as an offline coding-agent scenario; the Mango
HTTP contract, lifecycle, runtime, and assertions remain defined by this
repository's implementation and tests.

Source:
https://github.com/anthropics/claude-cookbooks/tree/main/managed_agents/example_data/iterate

The source fixture is MIT licensed. See `LICENSE` in this directory.
