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
  mdiViewGridOutline,
  mdiSourceRepository,
  mdiCurrencyUsd,
  mdiClipboardTextClockOutline,
  mdiCogOutline,
  mdiLogin,
  mdiContentCopy,
  mdiCheck,
  mdiChevronRight,
  mdiOpenInNew,
  mdiRefresh,
} from '@mdi/js';

import { siGithub } from 'simple-icons';

interface BrandIcon {
  path: string;
  hex: string;
  title: string;
}

// Forge brand logos, keyed by the forge kind an account/repo is configured
// with.
export const forgeIcon: Record<string, BrandIcon> = {
  github: siGithub,
};

