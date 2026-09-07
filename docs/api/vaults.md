---
title: Vaults and Credentials
description: Manage encrypted credentials for MCP connections.
slug: /api/vaults
---

# Vaults and Credentials

Vaults group write-only credentials used by MCP connections. The API is
enabled only when `MANGO_VAULT_KEYRING_FILE` points to a valid
operator-mounted keyring.

The thirteen operations cover Vault and Credential create/get/update/list/
archive/delete plus `mcp_oauth_validate`. Exact paths and request unions are in
the running server's `/openapi.yaml`.

## Secret boundary

Static bearer tokens, OAuth access and refresh tokens, and OAuth client secrets
never appear in public responses. PostgreSQL stores versioned AES-256-GCM
envelopes whose authenticated data binds ciphertext to its Vault, Credential,
and public authentication configuration. Archive purges encrypted payload
columns in the same transaction as the lifecycle change.

The API and worker load the same keyring. Credentials are decrypted only for a
matching outbound request and are never mounted in a Session sandbox or added
to model context.

## Session and MCP behavior

Session creation accepts ordered `vault_ids`. The first active Vault with a
credential whose canonical HTTPS endpoint matches the MCP server wins.
Credentials are re-resolved per request, so rotation and archive affect a
running Session. Expired OAuth grants refresh outside the database transaction
and commit encrypted token rotation under a row lock.

Authenticated redirects must keep the exact origin. Remote 401/403 responses
produce `mcp_authentication_failed_error` without terminating the Session.

Environment-variable secret egress and refresh-failure webhooks are not
implemented. See [Deployment model](../deployment.md) for keyring
configuration.
