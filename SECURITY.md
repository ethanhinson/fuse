# Security policy

## Reporting a vulnerability

Report privately through
[GitHub security advisories](https://github.com/ethanhinson/fuse/security/advisories/new).
Please do not open a public issue. Include the version from `fuse version`, what you did,
and what happened.

## Trust model

fuse runs a model that may act on hostile input, such as a poisoned file, web page or
tool result. These are the boundaries it is meant to hold:

- **A repository is untrusted.** A `.fuse.local.yml` in a repository can only tighten
  settings. It cannot choose the gateway, models, MCP servers, skills, instructions or
  limits. `.fuse/sandbox.local.yml` in a repository is ignored.
- **`~/.fuse` is trusted.** `~/.fuse/config.yml` and `~/.fuse/sandbox.local.yml` belong to
  the operator.
- **Bash runs in a container by default**, with an allowlisted environment, and fuse refuses
  to run bash if no container runtime is available.
- **Tools outside the workspace need approval.** Read and edit tools auto-approve only inside
  the workspace and configured roots. Credential locations under the home directory, and
  writes inside `.git` and `.fuse`, always need approval.

## Outside the boundary

- **`--approve-all` turns the permission gate off.** It is intended for an already isolated
  environment, such as a disposable container. Anything the agent can reach, it can change.
- **A `handler: host` sandbox runs bash directly on the machine**, with no containment.
- **MCP servers you configure run with your privileges**, and their output reaches the model.
- **The gateway you configure sees all prompts and tool results**, including file contents.
- **A model can be manipulated by what it reads.** Containment limits the damage. Review
  what an agent produces before trusting it.

## Supported versions

Only the latest release gets security fixes.
