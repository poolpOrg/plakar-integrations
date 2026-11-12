# n8n integration

Backs up an [n8n](https://n8n.io) instance into a Plakar snapshot through its
public REST API, and restores it the same way. The instance is exposed as a
virtual filesystem of JSON documents, one per resource:

```
n8n://
├── workflows/<workflow_id>.json         # nodes, connections, settings, static and pinned data, tags
├── tags/<tag_id>.json
├── variables/<variable_id>.json         # license-gated on the instance
├── projects/<project_id>.json           # license-gated on the instance
├── users/<user_id>.json                 # instance owner's key only
├── credentials/<credential_id>.json     # metadata only (name, type, owner); n8n 2.x
└── executions/<workflow_id>/<id>.json   # opt-in, with run data
```

Every document is the API's own JSON, pretty-printed so that snapshots diff
and read well.

## Usage

```sh
# API key from the environment
export N8N_API_KEY=eyJ...
plakar backup n8n://n8n.example.com

# instance served over plain http (local docker, for instance)
plakar backup n8n://localhost:5678 -o scheme=http

# behind a sub-path, with executions too
plakar backup n8n://example.com/n8n -o executions=true

# restore the workflows of a snapshot into an instance, re-activating the
# ones that were active
plakar restore -to n8n://n8n.example.com -o activate=true <snapid>:/workflows
```

The API key is created in n8n under *Settings → n8n API*. On n8n 2.x keys
carry scopes: a backup needs `workflow:list`, `workflow:read`, `tag:list`,
`user:list`, `credential:list` and, with `executions=true`, `execution:list`
and `execution:read`; a restore needs `workflow:create`, `workflow:update`,
`workflow:read`, `workflow:list`, `workflow:activate`, `workflowTags:update`,
`tag:list`, `tag:create`, `variable:list`, `variable:create`, `project:list`,
`project:create` and `credential:create`. A missing scope shows up as an
error on that section of the snapshot. The key may also ride in the location
userinfo (`n8n://eyJ...@n8n.example.com`), but `$N8N_API_KEY` or the `api_key`
option are preferred so it stays out of shell history and snapshot metadata.

### Importer options

| Option       | Description                                                          |
| ------------ | -------------------------------------------------------------------- |
| `api_key`    | n8n API key. Falls back to `$N8N_API_KEY`.                           |
| `scheme`     | `https` (default) or `http`.                                         |
| `executions` | `true` to also archive every execution with its data. Default off.   |
| `timeout`    | Per-request timeout, Go duration (default `60s`).                    |

### Exporter options

| Option     | Description                                                            |
| ---------- | ---------------------------------------------------------------------- |
| `api_key`  | n8n API key. Falls back to `$N8N_API_KEY`.                             |
| `scheme`   | `https` (default) or `http`.                                           |
| `activate` | `true` to re-activate workflows that were active in the snapshot.      |
| `timeout`  | Per-request timeout, Go duration (default `60s`).                      |

## What restore does

- **Workflows** are matched by id: a workflow with the same id on the target
  is replaced, otherwise a new one is created (under a new id). Because a
  created workflow gets a new id, a second restore into the same instance
  would not match it again; the exporter therefore refuses to create a
  workflow when the target already has one of that name, and says so. The
  read-only fields of the archived document (`id`, `active`, timestamps,
  `versionId`, `meta`, ...) are not sent back because the API rejects them.
  `description` and pinned data are restored on n8n 2.x, which accepts them,
  and dropped on n8n 1.x, which refuses them.
- **Tags** are matched by name and created when missing; a restored workflow
  gets the tags it had.
- Workflows are **not re-activated** unless `activate=true`: a restore should
  not start firing triggers on its own.
- **Variables** and **projects** are created when missing and left alone when
  present. A variable that exists with a different value is reported as an
  error rather than overwritten (the API has no update call).
- **Executions** and **users** cannot be written through the API and are
  skipped with a notice.
- **Credentials**: the API never hands the secret data back, so the backup
  holds their metadata only (on n8n 2.x, with the `credential:list` scope).
  A restore names each one as something to recreate by hand. The exporter
  does create credentials from documents that carry `{"name", "type",
  "data"}`, e.g. a decrypted `n8n export:credentials` run placed next to the
  backup.

## Notes

- Each section is independent: if listing one fails, that directory is
  recorded as an error and the rest of the backup still proceeds. The one
  exception is a section the instance's license does not include (variables
  and projects on the community edition): n8n says so in its answer, and the
  section is left out with a notice. A key that merely lacks the scope, or a
  non-owner key on `/users`, is an error in the snapshot like any other.
- Reads retry with exponential backoff on `429` and `5xx`, honouring
  `Retry-After`. Writes that create something are never retried: an answer
  lost after the write went through would duplicate it.
- Verified against n8n 2.47.3 (community edition, Docker image) for backup,
  restore into a fresh instance with re-activation, restore back onto the
  source, and the refusals above.
