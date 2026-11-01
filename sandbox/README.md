# Docker Agent sandbox kit (v3)

CI builds `docker/sbx-kit-docker-agent` from this checkout, for Linux amd64
and arm64. The kit uses the same binary and development tools as the legacy
`docker/docker-agent-sbx-templates` image, not an independently downloaded
release. The `kit` stage in `Dockerfile` builds on the `template` stage directly.
Neither artifact enables automatic binary updates.

| Tag | Source |
| --- | --- |
| `edge` | Current `main`, after the CI gate passes |
| `latest` | Most recent `v*` release |
| `1.2.3` | Matching `v1.2.3` release |

Use a v3-capable sbx, with provider credentials configured on the host:

```console
$ sbx run docker/sbx-kit-docker-agent:latest .
$ docker agent run --sandbox --sandbox-kit docker/sbx-kit-docker-agent:edge default
```

Pin `docker/sbx-kit-docker-agent@sha256:...` for reproducibility. Existing
sandboxes are not refreshed when a mutable tag moves. The default launch
runs the built-in `default` agent with autonomous tool permissions (`--yolo`)
inside the sandbox. Docker Agent's `--sandbox-kit` wrapper supplies its own
command and safety flags instead.

The kit declares optional, proxy-managed credentials for Anthropic, GitHub
and Copilot, Google, Mistral, Nebius, OpenAI, OpenRouter, and xAI. Docker Agent
does not forward host `GITHUB_TOKEN` or `GH_TOKEN`; configure GitHub credentials
through sbx like the other providers. Secrets stay in the host proxy; no
credentials are baked into the image. Runtime egress
covers those services, models.dev, and Docker Hub. Add mixins for additional
providers, package downloads, private registries, or other network access.
The kit also declares read-only shared skills, `/home/agent/AGENTS.md` context
(loaded alongside project instructions), headless prompts, and interactive
prompt/resume/continue commands. The built-in
agent consumes both skills and context; custom agents must enable them explicitly.

Cloud support depends on the sbx backend. Configure secrets through
`sbx --cloud secret`; Docker Agent does not forward host credentials or
upload the host workspace in cloud mode. Some backends drop optional
credential bindings for uploaded/assembled kits: a declaration alone does
not guarantee authenticated access. See the
[sandbox guide](../docs/configuration/sandbox/index.md#cloud-sandboxes).

## Build and validate

From the repository root, using a container or cloud Buildx builder:

```console
$ task kit
$ docker buildx build --builder kit-builder --file sandbox.yaml --output type=oci,dest=/tmp/docker-agent-kit,tar=false --provenance=mode=min .
$ go install github.com/docker/sandbox-kit-spec/v3/cmd/kit-tck@v3.0.0-m.11
$ kit-tck validate --layout /tmp/docker-agent-kit latest
```

`task kit` builds without pushing. The root [`sandbox.yaml`](../sandbox.yaml)
is the v3 descriptor; its build argument selects the kit content in the shared
Dockerfile automatically, including when sbx builds from source.
Set `GIT_TAG` and `GIT_COMMIT` to stamp a local build; CI sets them from the
checked-out ref. `KIT_VERSION` defaults to `0.0.0-dev`; CI uses the release
version or `0.0.0-dev.<commit>` for branch builds.
The descriptor's `version` describes the build, and does not claim a
versioned `provides` entry for development builds.

CI builds PRs without publishing tags, and pushes trusted builds by digest.
It validates the registry artifact with the pinned Kit TCK before the gate
passes; only then does the publish job promote tags. Building through the
v3 frontend validates the descriptor on PRs too. The legacy template and
the default `--sandbox` behavior remain unchanged.
