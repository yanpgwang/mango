# Coding-agent iterate fixture

`calc.py` and `test_calc.py` are adapted from Anthropic's public
[`CMA_iterate_fix_failing_tests` cookbook](https://github.com/anthropics/claude-cookbooks/blob/main/managed_agents/CMA_iterate_fix_failing_tests.ipynb)
and its
[`example_data/iterate` fixture](https://github.com/anthropics/claude-cookbooks/tree/main/managed_agents/example_data/iterate).

The runnable Python SDK example and Mango's deterministic/opt-in live service
tests share these fixtures. The tests use Python's standard-library `unittest`
instead of requiring pytest or installing packages inside the sandbox.
The source material is MIT licensed; the source license is retained here.
