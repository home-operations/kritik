<script lang="ts">
  import AppManifestFormFields from './AppManifestForm.svelte';
</script>

<section class="panel" aria-labelledby="op-app">
  <header class="panel-head"><h2 id="op-app">Create a GitHub App</h2></header>
  <div class="panel-body">
    <p class="field-hint">
      GitHub registers the App with the webhook, permissions and events kritik needs, and kritik adds it as a connection
      serving the account it belongs to. To serve more accounts, add them to the connection afterwards.
    </p>
    <AppManifestFormFields />
  </div>
</section>
