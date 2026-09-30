// The admin Configuration page's sections, which the command palette and
// the settings navigation both list. Rune-free so tests can import it.

export interface SettingEntry {
  label: string;
  // target selects the element to focus once its page shows it.
  target: string;
  keywords: string;
}

// The Configuration page's sections, in page order.
export const CONSOLE_SECTIONS: readonly SettingEntry[] = [
  { label: 'Setup', target: '#op-setup', keywords: 'checklist missing' },
  { label: 'Accounts', target: '#op-accounts', keywords: 'served usage' },
  { label: 'Instance settings', target: '#op-instance', keywords: 'environment sources defaults providers embedding' },
  { label: 'Instance configuration', target: '#op-config', keywords: 'spec json' },
  { label: 'Connections', target: '#op-connections', keywords: 'github app installations uninstall' },
  { label: 'Admin audit log', target: '#op-audit', keywords: 'history' },
];
