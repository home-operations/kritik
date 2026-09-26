// The config editor's model of a tenant spec: the JSON form of a tenant
// entry of the configuration file (see internal/webapi/secrets.go), split
// into the fields the form edits plus `rest`, every key it does not know,
// carried through unchanged so a save never drops a setting the form has no
// control for. Pure, so the form's request body follows from the draft
// alone.
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

export interface InstallationDraft {
  key: number;
  // The name the installation was loaded under, '' for a new one. The
  // server keeps a secret by the installation's name, so a renamed one
  // must not keep: it would adopt whatever is stored under the new name.
  origName: string;
  name: string;
  forge: Forge;
  host: string;
  account: string;
  // github: the app's credentials.
  clientId: string;
  clientIdFrom: SecretDraft;
  privateKey: SecretDraft;
  appWebhookSecret: SecretDraft;
  appRest: Obj;
  // gitlab and forgejo: a token.
  token: SecretDraft;
  webhookSecret: SecretDraft;
  gitToken: SecretDraft;
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

export interface TenantDraft {
  slug: string;
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
  installations: InstallationDraft[];
  repositories: RepositoryDraft[];
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

export function newSecret(mode: SecretMode): SecretDraft {
  return { wasSet: false, mode, value: '' };
}

export function newInstallation(): InstallationDraft {
  return installationOf({});
}

function installationOf(v: unknown): InstallationDraft {
  const o = obj(v);
  const app = obj(o.app);
  return {
    key: ++keys,
    origName: str(o.name),
    name: str(o.name),
    forge: (str(o.forge) || 'github') as Forge,
    host: str(o.host),
    account: str(o.account),
    clientId: str(app.clientId),
    clientIdFrom: secretOf(app.clientIdFrom, 'none'),
    privateKey: secretOf(app.privateKey, 'replace'),
    appWebhookSecret: secretOf(app.webhookSecret, 'generate'),
    appRest: take(app, 'clientId', 'clientIdFrom', 'privateKey', 'webhookSecret'),
    token: secretOf(o.token, 'replace'),
    webhookSecret: secretOf(o.webhookSecret, 'generate'),
    gitToken: secretOf(o.gitToken, 'none'),
    rest: take(o, 'name', 'forge', 'host', 'account', 'app', 'token', 'webhookSecret', 'gitToken'),
  };
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

export function draftOf(spec: Obj): TenantDraft {
  const o = obj(spec);
  const models = obj(o.models);
  const limits = obj(o.limits);
  return {
    slug: str(o.slug),
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
    installations: Array.isArray(o.installations) ? o.installations.map(installationOf) : [],
    repositories: Array.isArray(o.repositories) ? o.repositories.map(repositoryOf) : [],
    rest: take(o, 'slug', 'models', 'filter', 'forks', 'settle', 'limits', 'runner', 'installations', 'repositories'),
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

function nonEmpty(o: Obj): boolean {
  return Object.keys(o).length > 0;
}

// canKeep reports whether an installation's stored secrets may be kept:
// only while it still has the name it was loaded under.
export function canKeep(d: InstallationDraft): boolean {
  return d.origName !== '' && d.name.trim() === d.origName;
}

// hasTypedSecret reports whether any secret holds a value typed into the
// form.
export function hasTypedSecret(d: TenantDraft): boolean {
  return d.installations.some((x) =>
    [x.clientIdFrom, x.privateKey, x.appWebhookSecret, x.token, x.webhookSecret, x.gitToken].some(
      (sd) => sd.mode === 'replace' && sd.value !== '',
    ),
  );
}

function installationSpec(b: Builder, d: InstallationDraft, i: number): Obj {
  const p = `installations[${i}]`;
  const out: Obj = { ...d.rest };
  const keep = canKeep(d);
  const secret = (o: Obj, key: string, sd: SecretDraft, path: string, required: boolean) => {
    if (!keep && sd.mode === 'keep') b.fail(path, 'the installation was renamed: enter this secret again');
    b.secret(o, key, sd, path, required);
  };
  if (d.name.trim() === '') b.fail(`${p}.name`, 'a name is required');
  out.name = d.name.trim();
  out.forge = d.forge;
  set(out, 'host', d.host);
  if (/^http:\/\//i.test(d.host.trim())) b.fail(`${p}.host`, 'a dashboard installation must reach its forge over https');
  if (d.account.trim() === '') b.fail(`${p}.account`, 'an account is required');
  out.account = d.account.trim();
  if (d.forge === 'github') {
    const app: Obj = { ...d.appRest };
    set(app, 'clientId', d.clientId);
    secret(app, 'clientIdFrom', d.clientIdFrom, `${p}.app.clientIdFrom`, false);
    if (app.clientId === undefined && app.clientIdFrom === undefined) b.fail(`${p}.app.clientId`, 'set a client ID');
    secret(app, 'privateKey', d.privateKey, `${p}.app.privateKey`, true);
    secret(app, 'webhookSecret', d.appWebhookSecret, `${p}.app.webhookSecret`, true);
    out.app = app;
  } else {
    secret(out, 'token', d.token, `${p}.token`, true);
    secret(out, 'webhookSecret', d.webhookSecret, `${p}.webhookSecret`, true);
    secret(out, 'gitToken', d.gitToken, `${p}.gitToken`, false);
  }
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

export function buildSpec(d: TenantDraft, redact = false): Built {
  const b = new Builder(redact);
  const out: Obj = { ...d.rest };
  if (d.slug.trim() === '') b.fail('slug', 'a slug is required');
  out.slug = d.slug.trim();
  b.object(out, 'runner', d.runner, 'runner');
  out.installations = d.installations.map((x, i) => installationSpec(b, x, i));
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

// pathMatches reports whether a field at field is implicated by an error at
// err: the same path, one inside it, or one it is inside.
export function pathMatches(field: string, err: string): boolean {
  if (!err || !field) return false;
  const under = (a: string, b: string) => a === b || a.startsWith(`${b}.`) || a.startsWith(`${b}[`);
  return under(err, field) || under(field, err);
}

// hookName is the installation a generated secret's key
// ("installations[<name>].<key>") belongs to.
export function hookName(key: string): string {
  const m = /^installations\[(.*)\]\./.exec(key);
  return m ? m[1]! : '';
}
