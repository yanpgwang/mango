# MongoDB query example

Ask an agent which inventory items need reordering. The agent queries MongoDB
from a Docker container using Bash and Python; the launcher passes the database
connection string as an ordinary container environment variable.

See [Query MongoDB](../../docs/examples/mongodb-query.md) for setup, expected
results, configuration, and cleanup. Run the commands from the repository root.

- `main.go`: the application, using Mango's public Go SDK.
- `worker.go`: one Docker launch and the SDK worker entrypoint inside the container.
- `Dockerfile`: the worker image with Python, `pymongo`, and SRV support.
- `compose.yaml` and `seed.js`: an optional local MongoDB with synthetic inventory.

The application does not start Mango services or provision a model endpoint.
The example owns its launcher configuration; Mango's API, runtime, SDK, and
reference Docker launcher are unchanged.
