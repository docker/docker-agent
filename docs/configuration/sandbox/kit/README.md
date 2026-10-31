# Docker Agent workload kit (v3)

The maintained kit now lives in [`sandbox/`](../../../../sandbox/README.md).
CI publishes it as `docker/sbx-kit-docker-agent` using the binary built from
the same checkout as the legacy sandbox template.

With a v3-capable sbx:

```console
$ docker agent run --sandbox --sandbox-kit docker/sbx-kit-docker-agent:latest default
```

See the [kit source and build instructions](../../../../sandbox/README.md)
for local builds, capabilities, credentials, cloud limitations, and tags.
