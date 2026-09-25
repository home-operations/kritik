// Central icon registry. Importing only the names we use keeps the bundle
// small (Rollup tree-shakes @mdi/js and simple-icons to just these paths).
// Grows as later tasks add pages; keep this list scoped to what's actually
// referenced from src/.

export {
  mdiThemeLightDark,
  mdiWeatherNight,
  mdiWhiteBalanceSunny,
  mdiChevronDown,
  mdiMagnify,
  mdiAccountOutline,
  mdiSourcePull,
  mdiTrayFull,
  mdiKeyboardOutline,
  mdiLogout,
  mdiConsoleLine,
  mdiViewDashboardOutline,
  mdiSourceRepository,
  mdiCurrencyUsd,
  mdiClipboardTextClockOutline,
  mdiCogOutline,
  mdiLogin,
} from '@mdi/js';

import { siForgejo, siGithub, siGitlab } from 'simple-icons';

interface BrandIcon {
  path: string;
  hex: string;
  title: string;
}

// Forge brand logos, keyed by the forge kind a tenant/repo is configured
// with (github | gitlab | forgejo).
export const forgeIcon: Record<string, BrandIcon> = {
  github: siGithub,
  gitlab: siGitlab,
  forgejo: siForgejo,
};

