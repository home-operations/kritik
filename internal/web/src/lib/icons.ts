// Central icon registry. Importing only the names we use keeps the bundle
// small (Rollup tree-shakes @mdi/js and simple-icons to just these paths),
// so the list holds only what src/ references.

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
} from '@mdi/js';

import { siGithub } from 'simple-icons';

export const githubIcon = siGithub.path;
