<script lang="ts">
  import { getJSON, sendJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import { describe } from '../../manage';
  import { canAdmin } from '../../session.svelte';
  import { toast } from '../../toast.svelte';
  import type { CreateInviteRequest, Invite, Member, MemberRemoved, Members, TenantRole } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import Pill from '../../components/Pill.svelte';
  import Time from '../../components/Time.svelte';
  import Dialog from '../../components/Dialog.svelte';

  let { slug }: { slug: string } = $props();
  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}`);
  const res = new Resource(() => getJSON<Members>(`${base}/members`));
  $effect(() => {
    void res.load();
  });
  const admin = $derived(canAdmin(slug));

  // The last failure, announced in the page's live region as well as a toast.
  let failure = $state('');
  let busy = $state(false);

  async function run(label: string, f: () => Promise<string | undefined>): Promise<void> {
    busy = true;
    failure = '';
    try {
      const ok = await f();
      if (ok) toast(ok);
    } catch (err) {
      failure = `${label} failed: ${describe(err)}`;
      toast(failure, 'danger');
    } finally {
      busy = false;
      void res.load();
    }
  }

  const inviteGrant = (m: Member) => m.sources.find((s) => s.source === 'invite');

  function setRole(m: Member, role: TenantRole): Promise<void> {
    return run('Changing the role', async () => {
      await sendJSON('PATCH', `${base}/members/${encodeURIComponent(m.account.id)}`, { role });
      return `${m.account.displayName} is now ${role} by invite`;
    });
  }

  let removing = $state<Member | undefined>(undefined);
  let removeOpen = $state(false);

  function askRemove(m: Member): void {
    removing = m;
    removeOpen = true;
  }

  async function remove(): Promise<void> {
    const m = removing;
    removeOpen = false;
    if (!m) return;
    await run('Removing access', async () => {
      const r = await sendJSON<MemberRemoved | undefined>('DELETE', `${base}/members/${encodeURIComponent(m.account.id)}`);
      return r?.note ?? `Removed ${m.account.displayName}'s invite access`;
    });
  }

  let email = $state('');
  let role = $state<TenantRole>('member');
  let ttlHours = $state<number | null>(168);

  async function invite(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    const body: CreateInviteRequest = { email: email.trim(), role };
    if (ttlHours !== null) body.ttlHours = ttlHours;
    await run('Inviting', async () => {
      const v = await sendJSON<Invite>('POST', `${base}/invites`, body);
      email = '';
      return `Invited ${v.email} as ${v.role}`;
    });
  }

  function revoke(v: Invite): Promise<void> {
    return run('Revoking the invite', async () => {
      await sendJSON('DELETE', `${base}/invites/${encodeURIComponent(v.id)}`);
      return `Revoked the invite for ${v.email}`;
    });
  }
</script>

<div class="sr-only" aria-live="assertive">{failure}</div>

<StateView {res} retry={() => res.load()}>
  {#snippet children(d)}
    <section class="panel" aria-labelledby="admin-members">
      <header class="panel-head"><h2 id="admin-members">Members</h2></header>
      <p class="panel-body field-hint">
        Forge access is derived from the forge each time someone signs in; only access an invite granted can be changed here.
      </p>
      {#if failure}<p class="state-msg state-error">{failure}</p>{/if}
      <div class="table-wrap">
        <table class="data">
          <thead>
            <tr>
              <th scope="col">Account</th>
              <th scope="col">Role</th>
              <th scope="col">Sources</th>
              {#if admin}<th scope="col">Invite access</th>{/if}
            </tr>
          </thead>
          <tbody>
            {#each d.members as m (m.account.id)}
              {@const inv = inviteGrant(m)}
              <tr>
                <td>
                  <div>{m.account.displayName}</div>
                  <div class="small muted mono">{m.account.email}</div>
                </td>
                <td><Pill tone={m.role === 'admin' ? 'accent' : 'muted'} label={m.role} /></td>
                <td>
                  {#each m.sources as s (s.source)}
                    <span class="small" title={s.source === 'forge' ? 'derived from the forge at sign-in' : 'granted by an invite'}>
                      {s.source}: {s.role}
                    </span>{' '}
                  {/each}
                  {#if !inv}<div class="field-hint">from the forge at sign-in</div>{/if}
                </td>
                {#if admin}
                  <td>
                    {#if inv}
                      <label class="sr-only" for="role-{m.account.id}">Invite role for {m.account.displayName}</label>
                      <select
                        id="role-{m.account.id}"
                        value={inv.role}
                        disabled={busy}
                        onchange={(e) => setRole(m, e.currentTarget.value as TenantRole)}
                      >
                        <option value="member">member</option>
                        <option value="admin">admin</option>
                      </select>
                      <button class="btn btn-small btn-danger" disabled={busy} onclick={() => askRemove(m)}>Remove</button>
                    {:else}
                      <span class="muted">—</span>
                    {/if}
                  </td>
                {/if}
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </section>

    {#if d.invites !== null}
      <section class="panel" aria-labelledby="admin-invites">
        <header class="panel-head"><h2 id="admin-invites">Pending invites</h2></header>
        {#if d.invites.length === 0}
          <p class="state-msg">No pending invites.</p>
        {:else}
          <div class="table-wrap">
            <table class="data">
              <thead>
                <tr>
                  <th scope="col">Email</th>
                  <th scope="col">Role</th>
                  <th scope="col">Invited by</th>
                  <th scope="col">Expires</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                {#each d.invites as v (v.id)}
                  <tr>
                    <td class="mono">{v.email}</td>
                    <td>{v.role}</td>
                    <td>{v.createdBy?.displayName ?? '—'}</td>
                    <td><Time iso={v.expiresAt} /></td>
                    <td>
                      <button class="btn btn-small btn-danger" disabled={busy} onclick={() => revoke(v)} aria-label={`Revoke the invite for ${v.email}`}>
                        Revoke
                      </button>
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
        <form class="inline-form" onsubmit={invite}>
          <label class="field">
            <span>Email</span>
            <input type="email" autocomplete="off" bind:value={email} required />
          </label>
          <label class="field">
            <span>Role</span>
            <select bind:value={role}>
              <option value="member">member</option>
              <option value="admin">admin</option>
            </select>
          </label>
          <label class="field">
            <span>Expires after (hours, 1–720)</span>
            <input type="number" min="1" max="720" bind:value={ttlHours} />
          </label>
          <button type="submit" class="btn btn-primary" disabled={busy || email.trim() === ''}>Invite</button>
        </form>
      </section>
    {/if}
  {/snippet}
</StateView>

<Dialog bind:open={removeOpen} title="Remove invite access?">
  <p>
    {removing?.account.displayName} loses the access an invite granted. Access the forge grants is derived again at their next sign-in.
  </p>
  {#snippet footer()}
    <button class="btn" onclick={() => (removeOpen = false)}>Keep</button>
    <button class="btn btn-primary btn-danger" onclick={remove}>Remove access</button>
  {/snippet}
</Dialog>
