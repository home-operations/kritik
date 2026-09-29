<script lang="ts">
  // The files the account's reviews read (ADR-0017 §2.4): instructions they
  // follow and context files that explain the code, with where each is
  // named and the repositories that read it. Read-only: rules are files in
  // the repositories, named by the configuration or a .kritik.yaml.
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { accountApi, repoRoute } from '../links';
  import { isAdmin } from '../session.svelte';
  import { parseTokens, type TokenSpec } from '../tokensearch';
  import type { Rule, RuleKind, RuleSource } from '../types';
  import StateView from '../components/StateView.svelte';
  import TokenSearch from '../components/TokenSearch.svelte';
  import Icon from '../Icon.svelte';
  import { mdiTextBoxCheckOutline, mdiFileDocumentOutline } from '../icons';

  let { slug }: { slug: string } = $props();

  const res = new Resource(() => getJSON<Rule[]>(`${accountApi(slug)}/rules`));
  $effect(() => {
    void res.load();
  });

  const KINDS: Record<RuleKind, { label: string; icon: string; what: string }> = {
    instructions: { label: 'Instructions', icon: mdiTextBoxCheckOutline, what: 'what a review follows' },
    context: { label: 'Context', icon: mdiFileDocumentOutline, what: 'a file that explains the code' },
  };
  const SOURCES: Record<RuleSource, string> = {
    default: "kritik's default",
    env: 'Environment',
    file: 'Config file',
    dashboard: 'Instance configuration',
    defaults: 'Instance defaults',
    account: 'Account entry',
    repository: '.kritik.yaml',
  };

  const repoNames = $derived([...new Set((res.data ?? []).flatMap((r) => r.repositories))].sort());
  const specs = $derived<TokenSpec[]>([
    { key: 'repo', hint: 'a repository', values: repoNames },
    { key: 'kind', hint: 'instructions or context', values: ['instructions', 'context'] },
    { key: 'source', hint: 'where it is named', values: ['default', 'env', 'file', 'defaults', 'account', 'repository'] },
  ]);
  let text = $state('');
  let applied = $state(parseTokens('', []));

  function shown(rules: Rule[]): Rule[] {
    const { tokens, q } = applied;
    const needle = q?.toLowerCase();
    return rules.filter(
      (r) =>
        (!tokens.repo || r.repositories.includes(tokens.repo)) &&
        (!tokens.kind || r.kind === tokens.kind) &&
        (!tokens.source || r.source === tokens.source) &&
        (!needle || r.path.toLowerCase().includes(needle) || r.description.toLowerCase().includes(needle)),
    );
  }
</script>

<svelte:head><title>Rules · {slug} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Rules</h1>
      <p class="muted small">
        The files reviews read: instructions they follow, and context files that explain the code. They live in each repository, named
        by the configuration's <span class="mono">review.instructions</span> and <span class="mono">review.context</span>
        {#if isAdmin()}(<a href={href({ name: 'admin', slug, section: 'config' })}>Configuration</a>){/if}
        or by the repository's own <span class="mono">.kritik.yaml</span>.
      </p>
    </header>
    <div class="toolbar" role="search">
      <TokenSearch
        id="rule-search"
        label="Search rules"
        placeholder="Search, or filter by repo:, kind: or source:"
        {specs}
        bind:text
        ready={!!res.data}
        onapply={(p) => (applied = p)}
      />
    </div>
    <StateView
      {res}
      retry={() => res.load()}
      isEmpty={(d) => d.length === 0}
      empty="No rules yet: name instruction or context files under review.instructions and review.context, in the configuration or a repository's .kritik.yaml."
    >
      {#snippet children(rules)}
        {@const rows = shown(rules)}
        {#if rows.length === 0}
          <p class="state-msg">No rule matches.</p>
        {:else}
          <div class="table-wrap table-card">
            <table class="data rule-table">
              <thead>
                <tr>
                  <th scope="col">Rule</th>
                  <th scope="col">Applies to</th>
                  <th scope="col">Named in</th>
                  <th scope="col">Repositories</th>
                </tr>
              </thead>
              <tbody>
                {#each rows as r (`${r.kind}\u0000${r.path}\u0000${r.source}\u0000${r.description}\u0000${r.paths.join()}`)}
                  {@const k = KINDS[r.kind]}
                  <tr>
                    <td class="wrap">
                      <div class="rule-main">
                        <span class="rule-kind" title="{k.label}: {k.what}"><Icon path={k.icon} size={14} label={k.label} /></span>
                        <span class="rule-text">
                          <span class="mono rule-path">{r.path}</span>
                          {#if r.description}<span class="rule-sub">{r.description}</span>{/if}
                        </span>
                      </div>
                    </td>
                    <td>
                      {#if r.paths.length}
                        <span class="rule-globs">{#each r.paths as p (p)}<code>{p}</code>{/each}</span>
                      {:else}<span class="muted small">every change</span>{/if}
                    </td>
                    <td class="small">{SOURCES[r.source] ?? r.source}</td>
                    <td class="wrap" title={r.repositories.join('\n')}>
                      <div class="rule-repos">
                        {#each r.repositories.slice(0, 2) as name (name)}
                          <a class="mono small" href={href(repoRoute(slug, name))}>{name}</a>
                        {/each}
                        {#if r.repositories.length > 2}<span class="small muted">and {r.repositories.length - 2} more</span>{/if}
                      </div>
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      {/snippet}
    </StateView>
  </div>
</main>
