# Postgres with CloudNativePG

kritika keeps everything it knows in one Postgres database: reviews, findings,
the job queue, transcripts and the similar-code index. This page sets that
database up with [CloudNativePG](https://cloudnative-pg.io/docs/1.30/) (CNPG).
Any Postgres that meets the requirements below works; CNPG is what the
examples use.

## What kritika needs

- **Postgres 18 with VectorChord.** The `vchord`
  ([VectorChord](https://github.com/tensorchord/VectorChord)) and `vector`
  (pgvector, whose types it builds on) extensions must exist before the first
  start, and `vchord` must be in `shared_preload_libraries`.
- **Three roles**, and kritika refuses to start without the separation:
    - the **owner** runs migrations and applies the configuration on the
      leader. It owns the database and must not be a superuser;
    - the **application** role (`database.app.role`, `kritika_app` by
      default) serves everything else. It must not own the tables, so
      row-level security applies to it;
    - the **runner** role (`database.runner.role`, `kritika_runner` by
      default) is handed to runner Jobs and can only write its own run.
- **Direct or session-mode connections** for the owner and application
  roles. The leader is whichever replica holds a session advisory lock on its
  owner connection, and the dashboard's live updates and the job queue's
  notifications `LISTEN` on the application connection. A transaction-mode
  pooler, such as PgBouncer or a CNPG `Pooler` with `poolMode: transaction`,
  hands a session to other clients between transactions, which breaks both.

## The cluster

TensorChord publishes CNPG images with VectorChord built in,
[`ghcr.io/tensorchord/cloudnative-vectorchord`](https://github.com/tensorchord/cloudnative-vectorchord),
tagged `<postgres>-<vchord>`. Two instances with synchronous replication keep
every committed write through a failover:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: kritika-postgres
spec:
  instances: 2
  imageName: ghcr.io/tensorchord/cloudnative-vectorchord:18.6-1.1.1
  enableSuperuserAccess: false
  postgresql:
    shared_preload_libraries:
      - vchord
    # A commit waits for the standby while it is healthy, so a failover
    # loses nothing; with the standby down, writes go on without it.
    synchronous:
      method: any
      number: 1
      dataDurability: preferred
  primaryUpdateStrategy: unsupervised
  # Update the standby first, then switch over to it, rather than restart
  # the primary in place.
  primaryUpdateMethod: switchover
  storage:
    size: 10Gi
  bootstrap:
    initdb:
      database: kritika
      owner: kritika
      secret:
        name: kritika-postgres-credentials
```

- `dataDurability: preferred` relaxes the synchronous requirement while the
  standby is unavailable. `required` would block writes instead.
- `primaryUpdateMethod: switchover` rejects an update that changes the image
  and the Postgres parameters at once; apply them one at a time.
- The bootstrap owner, `kritika`, is the owner role.

A `Database` resource creates the extensions. The operator runs it as
superuser, so the owner never needs that privilege:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: kritika
spec:
  cluster:
    name: kritika-postgres
  name: kritika
  owner: kritika
  extensions:
    - name: vector
      ensure: present
    - name: vchord
      ensure: present
```

The application and runner roles are `DatabaseRole` resources, which CNPG
recommends over the Cluster's `managed.roles`
([declarative role management](https://cloudnative-pg.io/docs/1.30/declarative_role_management/)).
kritika's migrations grant them what they need:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: DatabaseRole
metadata:
  name: kritika-app
spec:
  cluster:
    name: kritika-postgres
  name: kritika_app
  login: true
  passwordSecret:
    name: kritika-postgres-app
  # Deleting this resource leaves the role, so a running kritika keeps its
  # connections.
  databaseRoleReclaimPolicy: retain
---
apiVersion: postgresql.cnpg.io/v1
kind: DatabaseRole
metadata:
  name: kritika-runner
spec:
  cluster:
    name: kritika-postgres
  name: kritika_runner
  login: true
  passwordSecret:
    name: kritika-postgres-runner
  databaseRoleReclaimPolicy: retain
```

## Secrets and the connection

Each role's Secret is a `kubernetes.io/basic-auth` Secret with `username`
and `password`, as CNPG writes it for a `DatabaseRole` and for the
bootstrap owner, and that is all kritika needs:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kritika-postgres-app
type: kubernetes.io/basic-auth
stringData:
  username: kritika_app
  password: <password>
```

The owner's Secret, `kritika-postgres-credentials`, and the runner's,
`kritika-postgres-runner`, look the same with their own role. A password
generator, such as an External Secrets `Password` generator, can produce all
three, and a password may hold any character: kritika builds the connection
from the host and the role's username and password, so nothing is escaped
into a URI.

Point the chart at the cluster's read-write Service and the three Secrets:

```yaml
database:
  host: kritika-postgres-rw
  owner:
    existingSecret: kritika-postgres-credentials
  app:
    existingSecret: kritika-postgres-app
  runner:
    existingSecret: kritika-postgres-runner
```

`database.port`, `database.name` and `database.sslmode` default to `5432`,
`kritika` and `require`; `database.connectTimeout` (`10s`) bounds a
connection attempt, so a dial to a Service address DNS still caches after a
redeploy fails fast and the retry looks the name up again, rather than
hanging for minutes. A Secret that holds a connection URI instead, such as
one a password generator templates, names its key in that role's `uriKey`,
and the URI is used as it is.

`database.app.role` and `database.runner.role` must name the roles the
Secrets log in as, if they are not `kritika_app` and `kritika_runner`.

## Connections

Postgres allows 100 connections unless `max_connections` says otherwise.
Each kritika replica keeps an application pool of up to 16 connections and
an owner pool of up to 4, plus its `LISTEN` connections, and every runner
Job opens a pool of its own, of which it uses one or two. Connections open
on demand, so an idle replica holds few. A `pool_max_conns` in a URI
replaces that pool's ceiling: raise the application pool's if jobs wait on
the pool, or raise `max_connections` under the Cluster's
`postgresql.parameters` for more replicas. CNPG's
`cnpg_backends_total` metric shows what each `application_name`
(`kritika-app`, `kritika-owner`, `kritika-listen`) holds.

## Failover and maintenance

When the primary changes, by a failover or a switchover, kritika's
connections drop and reconnect on their own. The replica that held the leader
lock loses it with its owner connection and steps down, and another takes it
within a few seconds; until then the leader's duties and job fetching pause.
No kritika pod restarts.

CNPG's disruption budget for the primary allows no eviction, so draining the
primary's node waits until the primary moves. Switch over first, with the
[`cnpg` kubectl plugin](https://cloudnative-pg.io/docs/1.30/kubectl-plugin/):

```sh
kubectl cnpg status kritika-postgres -n kritika
kubectl cnpg promote kritika-postgres kritika-postgres-2 -n kritika
```

## Backups

The configuration lives in git and the similar-code index rebuilds on its
own, but reviews, findings, feedback, usage and the audit log exist only in
the database. CNPG backs a cluster up two ways
([backups](https://cloudnative-pg.io/docs/1.30/backup/)):

- **Object storage** through the
  [Barman Cloud plugin](https://cloudnative-pg.io/plugin-barman-cloud/),
  which also archives WAL for point-in-time recovery. The Cluster's native
  `barmanObjectStore` does the same but is deprecated since CNPG 1.26.
- **Volume snapshots** through the storage class's CSI driver
  ([volume snapshots](https://cloudnative-pg.io/docs/1.30/appendixes/backup_volumesnapshot/)),
  with `backup.volumeSnapshot` on the Cluster and a `ScheduledBackup`. They
  stay in the same storage system, and point-in-time recovery still needs a
  WAL archive in object storage.

A replica is not a backup: it copies a mistake as fast as anything else.
