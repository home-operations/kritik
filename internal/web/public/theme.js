// Sets the theme class before the first paint: the bundle is a module, and
// modules run after the browser may already have painted the light default.
// A file rather than an inline script, which the CSP (script-src 'self')
// refuses. It reads the preference theme.svelte.ts stores.
(() => {
  let pref = 'auto';
  try {
    pref = localStorage.getItem('kritik-theme') ?? 'auto';
  } catch {
    // Storage blocked: follow the OS, as theme.svelte.ts does.
  }
  const dark = pref === 'dark' || (pref !== 'light' && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.classList.add(dark ? 'dark' : 'light');
})();
