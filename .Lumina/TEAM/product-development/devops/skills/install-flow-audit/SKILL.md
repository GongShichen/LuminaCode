---
name: Install Flow Audit
description: Verify build, install, resource copy, and uninstall behavior.
when-to-use: Use when setup, build, install, uninstall, packaging, resources, service lifecycle, or local environment paths change.
user-invocable: false
context: inline
---

Audit the product's actual distribution and local-development flow:

- Build artifacts, wrappers, entrypoints, package metadata, container images, or platform bundles.
- Resource copying, generated assets, templates, migrations, schemas, and runtime dependencies.
- User config preservation, secrets handling, environment variables, permissions, and rollback expectations.
- Cleanup of services, ports, sockets, databases, caches, logs, pid files, and background processes.
- Cross-platform behavior for supported OSes, shells, filesystems, path separators, and CPU architectures.
- Idempotency: repeated install/uninstall/setup should not corrupt user data or leave stale state.
- Clear verification commands and expected pass signals.
