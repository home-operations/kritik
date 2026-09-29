<script lang="ts">
  import { onMount } from 'svelte';
  import { basePath } from './base';
  import { getJSON, sendJSON, signinState, ApiError } from './api.svelte';
  import { session } from './session.svelte';
  import Icon from './Icon.svelte';
  import { mdiLogin, githubIcon } from './icons';
  import type { Me, SignInProvider } from './types';

  let providers = $state<SignInProvider[]>([]);
  let error = $state<string | undefined>(undefined);
  let loading = $state(true);

  const local = $derived(providers.find((p) => p.type === 'local'));
  const redirects = $derived(providers.filter((p) => p.type !== 'local'));

  let user = $state('');
  let password = $state('');
  let signingIn = $state(false);
  let localError = $state<string | undefined>(undefined);

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
    return type === 'github' ? githubIcon : mdiLogin;
  }

  function loginHref(p: SignInProvider): string {
    const returnTo = signinState.returnTo || '#/';
    return `${basePath}/auth/login/${encodeURIComponent(p.name)}?return_to=${encodeURIComponent(returnTo)}`;
  }

  // The shell sends a signed-in user back to where the 401 bounced them
  // from, so loading the session is all a successful sign-in needs.
  async function signIn(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    signingIn = true;
    localError = undefined;
    try {
      await sendJSON<void>('POST', '/auth/local', { user, password });
      session.me = await getJSON<Me>('/api/v1/me');
    } catch (err) {
      password = '';
      if (err instanceof ApiError && err.code === 'invalid_credentials') localError = 'Wrong username or password.';
      else if (err instanceof ApiError && err.code === 'too_many_attempts') localError = 'Too many failed attempts. Try again later.';
      else localError = err instanceof ApiError ? err.message : 'Sign-in failed.';
    } finally {
      signingIn = false;
    }
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
      {#if local}
        <form class="signin-local" aria-label="{local.displayName} sign-in" onsubmit={signIn}>
          <label class="field">
            <span>Username</span>
            <input autocomplete="username" required bind:value={user} />
          </label>
          <label class="field">
            <span>Password</span>
            <input type="password" autocomplete="current-password" required bind:value={password} />
          </label>
          <div aria-live="assertive">{#if localError}<p class="signin-error" role="alert">{localError}</p>{/if}</div>
          <button class="btn btn-primary signin-provider" type="submit" disabled={signingIn}>
            {signingIn ? 'Signing in…' : 'Sign in'}
          </button>
        </form>
      {/if}
      {#if redirects.length > 0}
        {#if local}<p class="signin-or">or</p>{/if}
        <div class="signin-providers">
          {#each redirects as p (p.name)}
            <a class="btn signin-provider" href={loginHref(p)}>
              <Icon path={iconFor(p.type)} size={16} />
              {p.displayName}
            </a>
          {/each}
        </div>
      {/if}
    {/if}
  </div>
</div>
