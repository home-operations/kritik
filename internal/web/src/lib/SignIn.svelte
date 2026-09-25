<script lang="ts">
  import { onMount } from 'svelte';
  import { basePath } from './base';
  import { getJSON, signinState, ApiError } from './api.svelte';
  import Icon from './Icon.svelte';
  import { mdiLogin, forgeIcon } from './icons';
  import type { SignInProvider } from './types';

  let providers = $state<SignInProvider[]>([]);
  let error = $state<string | undefined>(undefined);
  let loading = $state(true);

  onMount(() => {
    void load();
  });

  async function load(): Promise<void> {
    try {
      providers = await getJSON<SignInProvider[]>('/auth/providers');
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'failed to load sign-in providers';
    } finally {
      loading = false;
    }
  }

  function iconFor(type: SignInProvider['type']): string {
    return type === 'oidc' ? mdiLogin : (forgeIcon[type]?.path ?? mdiLogin);
  }

  function loginHref(p: SignInProvider): string {
    const returnTo = signinState.returnTo || '#/';
    return `${basePath}/auth/login/${encodeURIComponent(p.name)}?return_to=${encodeURIComponent(returnTo)}`;
  }
</script>

<div class="signin">
  <div class="signin-card">
    <img src="{basePath}/favicon.svg" width="40" height="40" alt="" />
    <h1>kritik</h1>
    <p class="signin-sub">Sign in to continue</p>

    {#if loading}
      <p class="signin-empty">Loading sign-in options…</p>
    {:else if error}
      <p class="signin-error">{error}</p>
    {:else if providers.length === 0}
      <p class="signin-empty">No sign-in providers configured.</p>
    {:else}
      <div class="signin-providers">
        {#each providers as p (p.name)}
          <a class="btn signin-provider" href={loginHref(p)}>
            <Icon path={iconFor(p.type)} size={16} />
            {p.displayName}
          </a>
        {/each}
      </div>
    {/if}
  </div>
</div>
