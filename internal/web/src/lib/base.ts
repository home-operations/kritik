// basePath is the URL prefix the UI (and its API calls) are served under:
// "" at the web server's root, "/kritik" when KRITIK_WEB_URL carries that
// path. The router lives in the hash, so the page's own path is always the
// mount point, optionally naming index.html.
export function deriveBasePath(pathname: string): string {
  return pathname.replace(/index\.html$/, '').replace(/\/+$/, '');
}

export const basePath = deriveBasePath(typeof location === 'undefined' ? '/' : location.pathname);
