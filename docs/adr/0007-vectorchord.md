# ADR-0007: the vector index is VectorChord

- **Status:** Proposed
- **Date:** 2026-09-25
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.8 (storage)
  and §2.9 (the index): `pgvector` stays as the type provider, the index
  access method becomes VectorChord's `vchordrq`, and `vchord` joins
  `vector` as a required extension.
- **Authors:** onedr0p.

## 1. Context

ADR-0002 indexes `index_chunks.embedding` (`halfvec`) with pgvector's HNSW
and relies on `hnsw.iterative_scan` so that a small repository inside a
large shared index still gets its full result count. That setting was
never wired into the query, so a review of a small repository can get
fewer than the requested neighbours today.

[VectorChord](https://github.com/tensorchord/VectorChord) (`vchord`) is a
Postgres extension that builds on pgvector's types and operators and
provides the `vchordrq` access method: RaBitQ quantisation with optional
partitioning, built in a fraction of HNSW's time and memory, with a
prefilter mode that evaluates a query's `WHERE` clause before computing
distances. It is what Immich moved to, TensorChord publishes a
CloudNativePG image for it (`ghcr.io/tensorchord/cloudnative-vectorchord`,
Postgres 18.6 with VectorChord 1.1.1 at the time of writing) and a scratch
image that mounts as a CloudNativePG image-volume extension, and the
operator running kritik's development cluster already uses it elsewhere.
kritik is unreleased, so the switch costs no migration of anyone's data.

## 2. Decision

- `index_chunks_embedding_idx` is `USING vchordrq (embedding
  halfvec_cosine_ops)` with default options: no partitioning, since the
  table stays far below the size at which VectorChord recommends `lists`,
  and the similarity query is unchanged (`ORDER BY embedding <=> $1
  LIMIT n`). `EnsureIndexSchema` checks the index's access method on every
  start and replaces one built with anything else, so a database created
  by an earlier build converts itself.
- The similarity query runs with `SET LOCAL vchordrq.prefilter = on`: its
  filters, the index generation and the tenant policy, are strict and
  cheap, which is when VectorChord recommends it, and with prefilter the
  index skips every chunk outside the repository's generation rather than
  ranking it first.
- Startup requires both `vchord` and `vector` in `pg_extension`. kritik
  still creates neither; on CloudNativePG the `Database` resource declares
  both and the `Cluster` loads `vchord` through `shared_preload_libraries`.
- The integration suite and CI run against
  `ghcr.io/tensorchord/vchord-postgres`.

## 3. Consequences

**Positive.** Full result counts for small repositories in a shared index
without an HNSW-specific setting; faster index builds on onboarding and
reindex; a prefilter that fits kritik's query exactly; the same extension
family the operator's other databases run.

**Negative.** A second required extension and a `shared_preload_libraries`
entry, so the stock CloudNativePG images no longer suffice: a deployment
needs TensorChord's image or the image-volume extension. VectorChord is
AGPL-3.0 / ELv2 licensed, which binds the database image, not kritik.

## 4. Alternatives considered

- **Keep HNSW and wire `hnsw.iterative_scan`.** Fixes the result count
  and needs no new extension; keeps HNSW's build cost and memory, and a
  second vector extension on the operator's clusters.
- **`vchordg` (VectorChord's graph index).** Closer to HNSW in behaviour;
  `vchordrq` is the one VectorChord documents as the default and the one
  with prefilter.
