// A search box's filter tokens, such as "repo:owner/name": reading them out
// of the box's text, writing the text back from them, and suggesting the
// token being typed. Rune-free so tests can import it.

export interface TokenSpec {
  key: string;
  hint: string;
  // values are what the token may name, matched in any case and written
  // as listed; with open set, any value is taken and values only suggest.
  values: readonly string[];
  open?: boolean;
}

export interface Parsed {
  tokens: Record<string, string>;
  // q is the text that is not a token.
  q?: string;
  // unknown are the tokens whose value is not among their spec's values.
  unknown: string[];
}

// parseTokens reads text; the last of a repeated token wins.
export function parseTokens(text: string, specs: readonly TokenSpec[]): Parsed {
  const tokens: Record<string, string> = {};
  const unknown: string[] = [];
  const free: string[] = [];
  for (const word of text.split(/\s+/).filter(Boolean)) {
    const colon = word.indexOf(':');
    const spec = colon > 0 ? specs.find((s) => s.key === word.slice(0, colon).toLowerCase()) : undefined;
    const value = word.slice(colon + 1);
    if (!spec || !value) {
      free.push(word);
      continue;
    }
    const known = spec.values.find((v) => v.toLowerCase() === value.toLowerCase());
    if (known) tokens[spec.key] = known;
    else if (spec.open) tokens[spec.key] = value;
    else unknown.push(word);
  }
  return free.length ? { tokens, q: free.join(' '), unknown } : { tokens, unknown };
}

// formatTokens writes the text for tokens, in the specs' order, then q.
export function formatTokens(specs: readonly TokenSpec[], tokens: Record<string, string | undefined>, q?: string): string {
  const words = specs.flatMap((s) => (tokens[s.key] ? [`${s.key}:${tokens[s.key]}`] : []));
  if (q) words.push(q);
  return words.join(' ');
}

export interface Suggestion {
  label: string;
  hint?: string;
  // text replaces the word being typed.
  text: string;
}

// suggest completes the last word of text: a token's key while it has no
// colon, then the token's values.
export function suggest(text: string, specs: readonly TokenSpec[]): Suggestion[] {
  const word = /\S*$/.exec(text)![0];
  const colon = word.indexOf(':');
  if (colon < 0) {
    const w = word.toLowerCase();
    return specs.filter((s) => s.key.startsWith(w)).map((s) => ({ label: `${s.key}:`, hint: s.hint, text: `${s.key}:` }));
  }
  const spec = specs.find((s) => s.key === word.slice(0, colon).toLowerCase());
  const partial = word.slice(colon + 1).toLowerCase();
  return (spec?.values ?? [])
    .filter((v) => v.toLowerCase().includes(partial) && v.toLowerCase() !== partial)
    .slice(0, 8)
    .map((v) => ({ label: `${spec!.key}:${v}`, text: `${spec!.key}:${v} ` }));
}

// complete replaces the last word of text with a suggestion.
export function complete(text: string, s: Suggestion): string {
  return text.replace(/\S*$/, s.text);
}
