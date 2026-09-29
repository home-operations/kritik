// The config editors' model of the instance spec and of an account's entry
// in it: their JSON (see internal/webapi/secrets.go), split into the fields
// a form edits plus `rest`, every key it does not know, carried through
// unchanged so a save never drops a setting the form has no control for.
// Pure, so a form's request body follows from the draft alone.
import type { Forge, ReviewMode, SecretInput } from './types';

type Obj = Record<string, unknown>;

// How a save treats one secret position: keep the stored value, replace it
// with the one typed, have the server generate one (webhook secrets only),
// or leave it unset.
export type SecretMode = 'keep' | 'replace' | 'generate' | 'none';

export interface SecretDraft {
  wasSet: boolean;
  mode: SecretMode;
  value: string;
}

// '' is "not set": the file's default applies.
export type TriBool = '' | 'true' | 'false';

export interface ConnectionDraft {
  key: number;
  // The name the connection was loaded under, '' for a new one. The
  // server keeps a secret by the connection's name, so a renamed one
  // must not keep: it would adopt whatever is stored under the new name.
  origName: string;
  name: string;
  forge: Forge;
  // accounts: one per line.
  accounts: string;
  // The App's credentials.
  clientId: string;
  clientIdFrom: SecretDraft;
  privateKey: SecretDraft;
  appWebhookSecret: SecretDraft;
  appRest: Obj;
  rest: Obj;
}

export type ProviderType = 'openrouter' | 'openai' | 'anthropic';

// A model provider, the instance's or an account's own: its key, for models
// it pays for.
export interface ProviderDraft {
  key: number;
  // The name and endpoint the provider was loaded under: the server keeps
  // a key by the provider's name, and only while its endpoint is the same.
  origName: string;
  origEndpoint: string;
  name: string;
  type: ProviderType;
  // '' is the type's own endpoint.
  baseUrl: string;
  apiKey: SecretDraft;
  rest: Obj;
}

export interface RepositoryDraft {
  key: number;
  name: string;
  enabled: TriBool;
  filter: string;
  // One glob per line.
  ignore: string;
  settle: string;
  mode: ReviewMode | '';
  // JSON text of the agent block, '' for none.
  agent: string;
  maxDeltaFiles: string;
  incrementalRest: Obj;
  // One per line.
  instructions: string;
  requireSuggestedFix: boolean;
  reviewRest: Obj;
  rest: Obj;
}

export interface AccountDraft {
  // The account the entry is for; the form does not change them.
  forge: Forge;
  name: string;
  reviewModel: string;
  fallbackModel: string;
  modelsRest: Obj;
  filter: string;
  forks: TriBool;
  settle: string;
  concurrency: string;
  reviewsPerDay: string;
  tokensPerMonth: string;
  limitsRest: Obj;
  // JSON text of the runner block, '' for none.
  runner: string;
  providers: ProviderDraft[];
  repositories: RepositoryDraft[];
  rest: Obj;
}

// The instance's embedder, which builds the similar-code index.
export interface EmbeddingDraft {
  // The endpoint it was loaded with: the server keeps its key only while
  // the endpoint is the same.
  origEndpoint: string;
  baseUrl: string;
  model: string;
  dims: string;
  apiKey: SecretDraft;
  rest: Obj;
}

// The instance spec as its form edits it: the connections, the
// instance's provider keys and its embedder, and everything else, the
// defaults and the accounts among it, as rest.
export interface InstanceDraft {
  connections: ConnectionDraft[];
  providers: ProviderDraft[];
  embedding: EmbeddingDraft | undefined;
  rest: Obj;
}

export interface SpecError {
  path: string;
  message: string;
}

let keys = 0;

function obj(v: unknown): Obj {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? { ...(v as Obj) } : {};
}

function str(v: unknown): string {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  return '';
}

function tri(v: unknown): TriBool {
  return v === true ? 'true' : v === false ? 'false' : '';
}

function lines(v: unknown): string {
  return Array.isArray(v) ? v.map(str).join('\n') : '';
}

function json(v: unknown): string {
  return v === undefined || v === null ? '' : JSON.stringify(v, null, 2);
}

function take(o: Obj, ...names: string[]): Obj {
  for (const n of names) delete o[n];
  return o;
}

// secretOf reads one secret position in any of the forms the API uses: a
// read's {"set": bool}, or a write's keep/value/generate (the JSON editor
// may hold either). Absent, it starts as fallback.
function secretOf(v: unknown, fallback: SecretMode): SecretDraft {
  const m = obj(v);
  if (m.set === true || m.keep === true) return { wasSet: true, mode: 'keep', value: '' };
  if (m.generate === true) return { wasSet: false, mode: 'generate', value: '' };
  if (typeof m.value === 'string') return { wasSet: false, mode: 'replace', value: m.value };
  return { wasSet: false, mode: fallback, value: '' };
}

export function newConnection(): ConnectionDraft {
  return connectionOf({});
}

function providerOf(name: string, v: unknown): ProviderDraft {
  const o = obj(v);
  const type = (str(o.type) || 'openrouter') as ProviderType;
  const baseUrl = str(o.baseUrl);
  return {
    key: ++keys,
    origName: name,
    origEndpoint: providerEndpoint({ type, baseUrl }),
    name,
    type,
    baseUrl,
    apiKey: secretOf(o.apiKey, 'replace'),
    rest: take(o, 'type', 'baseUrl', 'apiKey'),
  };
}

// canKeepKey says whether the provider may keep its stored key: it has its
// name and endpoint still.
export function canKeepKey(d: ProviderDraft): boolean {
  return d.origName !== '' && d.name.trim() === d.origName && providerEndpoint(d) === d.origEndpoint;
}

export function newProvider(): ProviderDraft {
  return providerOf('', {});
}

// providerEndpoint is where a provider's key goes, as the server compares
// it when keeping the key: its type and base URL, in any case, without a
// trailing slash.
export function providerEndpoint(d: Pick<ProviderDraft, 'type' | 'baseUrl'>): string {
  return `${d.type} ${d.baseUrl.trim().toLowerCase().replace(/\/+$/, '')}`;
}

function connectionOf(v: unknown): ConnectionDraft {
  const o = obj(v);
  const app = obj(o.app);
  return {
    key: ++keys,
    origName: str(o.name),
    name: str(o.name),
    forge: (str(o.forge) || 'github') as Forge,
    accounts: lines(o.accounts),
    clientId: str(app.clientId),
    clientIdFrom: secretOf(app.clientIdFrom, 'none'),
    privateKey: secretOf(app.privateKey, 'replace'),
    appWebhookSecret: secretOf(app.webhookSecret, 'generate'),
    appRest: take(app, 'clientId', 'clientIdFrom', 'privateKey', 'webhookSecret'),
    rest: take(o, 'name', 'forge', 'accounts', 'app'),
  };
}

// embeddingEndpoint is where the embedder's key goes, as the server
// compares it when keeping the key: its base URL, in any case, without a
// trailing slash.
function embeddingEndpoint(baseUrl: string): string {
  return baseUrl.trim().toLowerCase().replace(/\/+$/, '');
}

function embeddingOf(v: unknown): EmbeddingDraft {
  const o = obj(v);
  return {
    origEndpoint: embeddingEndpoint(str(o.baseUrl)),
    baseUrl: str(o.baseUrl),
    model: str(o.model),
    dims: str(o.dims),
    apiKey: secretOf(o.apiKey, 'replace'),
    rest: take(o, 'baseUrl', 'model', 'dims', 'apiKey'),
  };
}

export function newEmbedding(): EmbeddingDraft {
  return embeddingOf({});
}

// canKeepEmbeddingKey says whether the embedder may keep its stored key: it
// has its endpoint still.
export function canKeepEmbeddingKey(d: EmbeddingDraft): boolean {
  return embeddingEndpoint(d.baseUrl) === d.origEndpoint;
}

export function newRepository(): RepositoryDraft {
  return repositoryOf({});
}

function repositoryOf(v: unknown): RepositoryDraft {
  const o = obj(v);
  const review = obj(o.review);
  const inc = obj(o.incremental);
  return {
    key: ++keys,
    name: str(o.name),
    enabled: tri(o.enabled),
    filter: str(o.filter),
    ignore: lines(o.ignore),
    settle: str(o.settle),
    mode: str(o.mode) as ReviewMode | '',
    agent: json(o.agent),
    maxDeltaFiles: str(inc.maxDeltaFiles),
    incrementalRest: take(inc, 'maxDeltaFiles'),
    instructions: lines(review.instructions),
    requireSuggestedFix: review.requireSuggestedFix === true,
    reviewRest: take(review, 'instructions', 'requireSuggestedFix'),
    rest: take(o, 'name', 'enabled', 'filter', 'ignore', 'settle', 'mode', 'agent', 'incremental', 'review'),
  };
}

export function draftOf(spec: Obj): AccountDraft {
  const o = obj(spec);
  const models = obj(o.models);
  const limits = obj(o.limits);
  return {
    forge: (str(o.forge) || 'github') as Forge,
    name: str(o.name),
    reviewModel: str(models.review),
    fallbackModel: str(models.fallback),
    modelsRest: take(models, 'review', 'fallback'),
    filter: str(o.filter),
    forks: tri(o.forks),
    settle: str(o.settle),
    concurrency: str(limits.concurrency),
    reviewsPerDay: str(limits.reviewsPerDay),
    tokensPerMonth: str(limits.tokensPerMonth),
    limitsRest: take(limits, 'concurrency', 'reviewsPerDay', 'tokensPerMonth'),
    runner: json(o.runner),
    providers: Object.entries(obj(o.providers)).map(([name, v]) => providerOf(name, v)),
    repositories: Array.isArray(o.repositories) ? o.repositories.map(repositoryOf) : [],
    rest: take(o, 'forge', 'name', 'models', 'filter', 'forks', 'settle', 'limits', 'runner', 'providers', 'repositories'),
  };
}

// keptSecrets is v with every secret a read shows as {"set": true} kept,
// and every one it shows as {"set": false} left out, so a part of the spec
// no form edits saves as it is stored.
function keptSecrets(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(keptSecrets);
  if (typeof v !== 'object' || v === null) return v;
  const o = v as Obj;
  const keys = Object.keys(o);
  if (keys.length === 1 && keys[0] === 'set' && typeof o.set === 'boolean') return o.set ? { keep: true } : undefined;
  const out: Obj = {};
  for (const [k, x] of Object.entries(o)) {
    const kept = keptSecrets(x);
    if (kept !== undefined) out[k] = kept;
  }
  return out;
}

export function instanceDraftOf(spec: Obj): InstanceDraft {
  const o = obj(spec);
  return {
    connections: Array.isArray(o.connections) ? o.connections.map(connectionOf) : [],
    providers: Object.entries(obj(o.providers)).map(([name, v]) => providerOf(name, v)),
    embedding: o.embedding ? embeddingOf(o.embedding) : undefined,
    rest: obj(keptSecrets(take(o, 'connections', 'providers', 'embedding'))),
  };
}

// Builder collects the spec and the first problem found. lenient keeps
// going past a problem (for the JSON view), and redact writes a typed secret
// as keep when there is one stored, or an empty value otherwise, so the
// JSON view never shows what was typed into a password field.
class Builder {
  error: SpecError | undefined;
  constructor(readonly redact: boolean) {}

  fail(path: string, message: string): void {
    this.error ??= { path, message };
  }

  secret(out: Obj, key: string, d: SecretDraft, path: string, required: boolean): void {
    let v: SecretInput | undefined;
    switch (d.mode) {
      case 'keep':
        v = { keep: true };
        break;
      case 'generate':
        v = { generate: true };
        break;
      case 'replace':
        if (this.redact) v = d.wasSet ? { keep: true } : { value: '' };
        else if (d.value === '') this.fail(path, 'enter the new value');
        else v = { value: d.value };
        break;
      case 'none':
        if (required) this.fail(path, 'this secret is required');
    }
    if (v) out[key] = v;
  }

  int(out: Obj, key: string, s: string, path: string): void {
    const t = s.trim();
    if (t === '') return;
    if (!/^\d+$/.test(t)) {
      this.fail(path, 'must be a whole number');
      out[key] = t;
      return;
    }
    out[key] = Number(t);
  }

  object(out: Obj, key: string, text: string, path: string): void {
    const t = text.trim();
    if (t === '') return;
    try {
      const v: unknown = JSON.parse(t);
      if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new Error('not an object');
      out[key] = v;
    } catch {
      this.fail(path, 'must be a JSON object');
    }
  }
}

function set(out: Obj, key: string, v: string): void {
  const t = v.trim();
  if (t !== '') out[key] = t;
}

function list(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '');
}

// accountKey is an accounts draft as the server compares it when keeping a
// secret: a set, in any case.
export function accountKey(text: string): string {
  return [...new Set(list(text).map((a) => a.toLowerCase()))].sort().join(',');
}

function nonEmpty(o: Obj): boolean {
  return Object.keys(o).length > 0;
}

// canKeep reports whether a connection's stored secrets may be kept:
// only while it still has the name it was loaded under.
export function canKeep(d: ConnectionDraft): boolean {
  return d.origName !== '' && d.name.trim() === d.origName;
}

// hasTypedSecret reports whether any secret holds a value typed into the
// form.
export function hasTypedSecret(d: { providers: ProviderDraft[]; connections?: ConnectionDraft[]; embedding?: EmbeddingDraft }): boolean {
  const typed = (sd: SecretDraft) => sd.mode === 'replace' && sd.value !== '';
  return (
    (d.connections ?? []).some((x) => [x.clientIdFrom, x.privateKey, x.appWebhookSecret].some(typed)) ||
    d.providers.some((x) => typed(x.apiKey)) ||
    (d.embedding !== undefined && typed(d.embedding.apiKey))
  );
}

const providerName = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// providerPath is where a provider's fields are, as the server names them.
export function providerPath(d: ProviderDraft): string {
  return `providers.${d.name.trim()}`;
}

function providersSpec(b: Builder, list: ProviderDraft[]): Obj {
  const out: Obj = {};
  for (const d of list) {
    const name = d.name.trim();
    const p = providerPath(d);
    if (!providerName.test(name)) b.fail(`${p}.name`, 'a name is lowercase letters, digits and hyphens');
    else if (name in out) b.fail(`${p}.name`, 'another provider key has this name');
    if (d.baseUrl.trim() !== '' && !/^https:\/\//i.test(d.baseUrl.trim())) b.fail(`${p}.baseUrl`, 'a provider endpoint must be https');
    if (d.apiKey.mode === 'keep' && !canKeepKey(d)) b.fail(`${p}.apiKey`, 'the name, type or endpoint changed: enter the key again');
    const o: Obj = { ...d.rest, type: d.type };
    set(o, 'baseUrl', d.baseUrl);
    b.secret(o, 'apiKey', d.apiKey, `${p}.apiKey`, true);
    out[name] = o;
  }
  return out;
}

function connectionSpec(b: Builder, d: ConnectionDraft, i: number): Obj {
  const p = `connections[${i}]`;
  const out: Obj = { ...d.rest };
  const keep = canKeep(d);
  const secret = (o: Obj, key: string, sd: SecretDraft, path: string, required: boolean) => {
    if (!keep && sd.mode === 'keep') b.fail(path, 'the connection was renamed: enter this secret again');
    b.secret(o, key, sd, path, required);
  };
  if (d.name.trim() === '') b.fail(`${p}.name`, 'a name is required');
  out.name = d.name.trim();
  out.forge = d.forge;
  const accounts = list(d.accounts);
  if (accounts.length === 0) b.fail(`${p}.accounts`, 'list at least one account');
  out.accounts = accounts;
  const app: Obj = { ...d.appRest };
  set(app, 'clientId', d.clientId);
  secret(app, 'clientIdFrom', d.clientIdFrom, `${p}.app.clientIdFrom`, false);
  if (app.clientId === undefined && app.clientIdFrom === undefined) b.fail(`${p}.app.clientId`, 'set a client ID');
  secret(app, 'privateKey', d.privateKey, `${p}.app.privateKey`, true);
  secret(app, 'webhookSecret', d.appWebhookSecret, `${p}.app.webhookSecret`, true);
  out.app = app;
  return out;
}

function embeddingSpec(b: Builder, d: EmbeddingDraft): Obj {
  const out: Obj = { ...d.rest };
  if (d.baseUrl.trim() === '') b.fail('embedding.baseUrl', 'an endpoint is required');
  out.baseUrl = d.baseUrl.trim();
  if (d.model.trim() === '') b.fail('embedding.model', 'a model is required');
  out.model = d.model.trim();
  if (d.dims.trim() === '') b.fail('embedding.dims', 'a dimension is required');
  b.int(out, 'dims', d.dims, 'embedding.dims');
  if (d.apiKey.mode === 'keep' && !canKeepEmbeddingKey(d)) b.fail('embedding.apiKey', 'the endpoint changed: enter the key again');
  b.secret(out, 'apiKey', d.apiKey, 'embedding.apiKey', true);
  return out;
}

function repositorySpec(b: Builder, d: RepositoryDraft, i: number): Obj {
  const p = `repositories[${i}]`;
  const out: Obj = { ...d.rest };
  if (d.name.trim() === '') b.fail(`${p}.name`, 'a name is required');
  out.name = d.name.trim();
  if (d.enabled !== '') out.enabled = d.enabled === 'true';
  set(out, 'filter', d.filter);
  const ignore = list(d.ignore);
  if (ignore.length) out.ignore = ignore;
  set(out, 'settle', d.settle);
  if (d.mode) out.mode = d.mode;
  b.object(out, 'agent', d.agent, `${p}.agent`);
  const inc: Obj = { ...d.incrementalRest };
  b.int(inc, 'maxDeltaFiles', d.maxDeltaFiles, `${p}.incremental`);
  if (nonEmpty(inc)) out.incremental = inc;
  const review: Obj = { ...d.reviewRest };
  const instructions = list(d.instructions);
  if (instructions.length) review.instructions = instructions;
  if (d.requireSuggestedFix) review.requireSuggestedFix = true;
  if (nonEmpty(review)) out.review = review;
  return out;
}

export interface Built {
  spec: Obj;
  error: SpecError | undefined;
}

export function buildSpec(d: AccountDraft, redact = false): Built {
  const b = new Builder(redact);
  const out: Obj = { ...d.rest, forge: d.forge, name: d.name };
  b.object(out, 'runner', d.runner, 'runner');
  if (d.providers.length) out.providers = providersSpec(b, d.providers);
  const models: Obj = { ...d.modelsRest };
  set(models, 'review', d.reviewModel);
  set(models, 'fallback', d.fallbackModel);
  if (nonEmpty(models)) out.models = models;
  set(out, 'filter', d.filter);
  if (d.forks !== '') out.forks = d.forks === 'true';
  const limits: Obj = { ...d.limitsRest };
  b.int(limits, 'concurrency', d.concurrency, 'limits.concurrency');
  b.int(limits, 'reviewsPerDay', d.reviewsPerDay, 'limits.reviewsPerDay');
  b.int(limits, 'tokensPerMonth', d.tokensPerMonth, 'limits.tokensPerMonth');
  if (nonEmpty(limits)) out.limits = limits;
  if (d.repositories.length) out.repositories = d.repositories.map((x, i) => repositorySpec(b, x, i));
  set(out, 'settle', d.settle);
  return { spec: out, error: b.error };
}

export function buildInstanceSpec(d: InstanceDraft, redact = false): Built {
  const b = new Builder(redact);
  const out: Obj = { ...d.rest };
  if (d.providers.length) out.providers = providersSpec(b, d.providers);
  if (d.embedding) out.embedding = embeddingSpec(b, d.embedding);
  if (d.connections.length) out.connections = d.connections.map((x, i) => connectionSpec(b, x, i));
  return { spec: out, error: b.error };
}

// pathMatches reports whether a field at field is implicated by an error at
// err: the same path, one inside it, or one it is inside.
export function pathMatches(field: string, err: string): boolean {
  if (!err || !field) return false;
  const under = (a: string, b: string) => a === b || a.startsWith(`${b}.`) || a.startsWith(`${b}[`);
  return under(err, field) || under(field, err);
}

// hookName is the connection a generated secret's key
// ("connections[<name>].<key>") belongs to.
export function hookName(key: string): string {
  const m = /^connections\[(.*)\]\./.exec(key);
  return m ? m[1]! : '';
}
