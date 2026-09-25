// Central icon registry. Importing only the names we use keeps the bundle
// small (Rollup tree-shakes @mdi/js and simple-icons to just these paths).
// Grows as later tasks add pages; keep this list scoped to what's actually
// referenced from src/.

export {
  mdiThemeLightDark,
  mdiWeatherNight,
  mdiWhiteBalanceSunny,
  mdiAlert,
  mdiAlertCircleOutline,
  mdiCheck,
  mdiCheckCircle,
  mdiCheckCircleOutline,
  mdiCircleOutline,
  mdiCloseCircleOutline,
  mdiClose,
  mdiChevronLeft,
  mdiChevronRight,
  mdiChevronDown,
  mdiChevronUp,
  mdiMagnify,
  mdiRefresh,
  mdiAccountOutline,
  mdiSourceBranch,
  mdiSourcePull,
  mdiSourceFork,
  mdiTrayFull,
  mdiKeyboardOutline,
  mdiLoading,
  mdiLogout,
  mdiCog,
  mdiConsoleLine,
  mdiOpenInNew,
  mdiClockOutline,
  mdiHistory,
  mdiPackageVariantClosed,
  mdiScaleBalance,
  mdiViewDashboardOutline,
  mdiSourceRepository,
  mdiCurrencyUsd,
  mdiClipboardTextClockOutline,
  mdiCogOutline,
  mdiLogin,
} from '@mdi/js';

import { siDiscord, siForgejo, siGithub, siGitlab } from 'simple-icons';

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

// The GitHub mark for the "kritik on GitHub" footer link (kritik is hosted
// there, independent of which forge a reviewed repo lives on).
export const githubMark: BrandIcon = siGithub;
export const discordMark: BrandIcon = siDiscord;
export const KRITIK_REPO_URL = 'https://github.com/home-operations/kritik';
export const DISCORD_URL = 'https://discord.gg/home-operations';
export const LICENSE_URL = 'https://github.com/home-operations/kritik/blob/main/LICENSE';
